// A small HTTP/1.1 server on net: keep-alive, pipelining (answers are batched until the input runs dry),
// Content-Length and chunked request bodies, Expect: 100-continue. Handlers run under one lock.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

type Request struct {
	Method string
	// Path is the decoded path without the query string.
	Path string
	// Query is the raw query string (no leading '?').
	Query string
	// Headers has lower-cased names; repeated headers are joined with ", ".
	Headers map[string]string
	Body    []byte
	Host    string
}

func (r *Request) Header(name string) (string, bool) {
	v, ok := r.Headers[name]
	return v, ok
}

type Response struct {
	Status int
	Type   string
	Body   []byte
	// Extra holds header lines, each formatted as "Name: value\r\n".
	Extra string
}

type Handler func(*Request) *Response

var reasons = map[int]string{
	100: "Continue",
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	301: "Moved Permanently",
	304: "Not Modified",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	410: "Gone",
	411: "Length Required",
	413: "Payload Too Large",
	415: "Unsupported Media Type",
	422: "Unprocessable Entity",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
	501: "Not Implemented",
}

const maxHeaderBytes = 64 * 1024
const maxBodyBytes = 512 * 1024 * 1024

var requestCounter uint64 = 0x5a3c00
var counterMu sync.Mutex

func nextRequestID() uint64 {
	counterMu.Lock()
	requestCounter++
	v := requestCounter
	counterMu.Unlock()
	return v
}

func decodePath(raw string) string {
	if strings.IndexByte(raw, '%') < 0 {
		return raw
	}
	out, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return out
}

// writeResponse appends one serialized response to w.
func writeResponse(w *bufio.Writer, resp *Response, head, close bool) {
	status := resp.Status
	reason, ok := reasons[status]
	if !ok {
		reason = "Unknown"
	}
	var hb [512]byte
	h := hb[:0]
	h = append(h, "HTTP/1.1 "...)
	h = strconv.AppendInt(h, int64(status), 10)
	h = append(h, ' ')
	h = append(h, reason...)
	h = append(h, "\r\nContent-Type: "...)
	h = append(h, resp.Type...)
	h = append(h, "\r\nX-GitHub-Request-Id: C0DE:"...)
	h = append(h, strings.ToUpper(strconv.FormatUint(nextRequestID(), 16))...)
	h = append(h, ":F0RGE\r\n"...)
	if status != 204 && status != 304 {
		h = append(h, "Content-Length: "...)
		h = strconv.AppendInt(h, int64(len(resp.Body)), 10)
		h = append(h, "\r\n"...)
	}
	h = append(h, resp.Extra...)
	if close {
		h = append(h, "Connection: close\r\n"...)
	}
	h = append(h, "\r\n"...)
	w.Write(h)
	if head || status == 204 || status == 304 || len(resp.Body) == 0 {
		return
	}
	w.Write(resp.Body)
}

type parseError struct {
	status int
	msg    string
}

func (e *parseError) Error() string { return e.msg }

func errorBody(status int, message string) *Response {
	return &Response{Status: status, Type: jsonType, Body: toJSON(O("message", message))}
}

// readHead reads the request line and headers (through the blank line).
func readHead(r *bufio.Reader) ([]string, error) {
	var lines []string
	total := 0
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A very long line: accumulate it.
			buf := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				buf = append(buf, line...)
				if len(buf) > maxHeaderBytes {
					return nil, &parseError{431, "headers too large"}
				}
			}
			line = buf
		}
		if err != nil {
			if len(lines) == 0 && len(line) == 0 {
				return nil, io.EOF
			}
			return nil, err
		}
		total += len(line)
		if total > maxHeaderBytes {
			return nil, &parseError{431, "headers too large"}
		}
		text := strings.TrimRight(string(line), "\r\n")
		if text == "" {
			if len(lines) == 0 {
				// Tolerate stray blank lines between requests.
				continue
			}
			return lines, nil
		}
		lines = append(lines, text)
	}
}

func readChunked(r *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.TrimSpace(strings.SplitN(strings.TrimRight(line, "\r\n"), ";", 2)[0])
		size, perr := strconv.ParseInt(sizeText, 16, 64)
		if perr != nil || size < 0 {
			return nil, &parseError{400, "bad chunked body"}
		}
		if size == 0 {
			for {
				t, err := r.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					if body == nil {
						body = []byte{}
					}
					return body, nil
				}
			}
		}
		if int64(len(body))+size > maxBodyBytes {
			return nil, &parseError{413, "body too large"}
		}
		start := len(body)
		body = append(body, make([]byte, size)...)
		if _, err := io.ReadFull(r, body[start:]); err != nil {
			return nil, err
		}
		var crlf [2]byte
		if _, err := io.ReadFull(r, crlf[:1]); err != nil {
			return nil, err
		}
		if crlf[0] == '\r' {
			if _, err := io.ReadFull(r, crlf[1:2]); err != nil {
				return nil, err
			}
		}
	}
}

func serveConn(conn net.Conn, handler Handler, lock *sync.Mutex) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	rd, wr, release := connIO(conn)
	defer release()
	r := bufio.NewReaderSize(rd, 64*1024)
	w := bufio.NewWriterSize(wr, 64*1024)
	fail := func(status int, msg string) {
		writeResponse(w, errorBody(status, msg), false, true)
		w.Flush()
	}
	for {
		if r.Buffered() == 0 && w.Buffered() > 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
		lines, err := readHead(r)
		if err != nil {
			var pe *parseError
			if errors.As(err, &pe) {
				fail(pe.status, pe.msg)
			} else {
				w.Flush()
			}
			return
		}
		requestLine := lines[0]
		firstSpace := strings.IndexByte(requestLine, ' ')
		lastSpace := strings.LastIndexByte(requestLine, ' ')
		if firstSpace <= 0 || lastSpace <= firstSpace {
			fail(400, "bad request line")
			return
		}
		method := strings.ToUpper(requestLine[:firstSpace])
		target := requestLine[firstSpace+1 : lastSpace]
		version := requestLine[lastSpace+1:]
		headers := make(map[string]string, len(lines))
		for _, line := range lines[1:] {
			colon := strings.IndexByte(line, ':')
			if colon <= 0 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(line[:colon]))
			value := strings.TrimSpace(line[colon+1:])
			if existing, ok := headers[name]; ok {
				headers[name] = existing + ", " + value
			} else {
				headers[name] = value
			}
		}
		expectContinue := strings.ToLower(headers["expect"]) == "100-continue"
		var body []byte
		if te, ok := headers["transfer-encoding"]; ok && strings.Contains(strings.ToLower(te), "chunked") {
			if expectContinue && r.Buffered() == 0 {
				w.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
				w.Flush()
			}
			body, err = readChunked(r)
			if err != nil {
				var pe *parseError
				if errors.As(err, &pe) {
					fail(pe.status, pe.msg)
				} else {
					w.Flush()
				}
				return
			}
		} else if cl, ok := headers["content-length"]; ok {
			length, ok := jsParseInt(cl)
			if !ok || length < 0 {
				fail(400, "bad content-length")
				return
			}
			if length > maxBodyBytes {
				fail(413, "body too large")
				return
			}
			if length > 0 {
				if expectContinue && r.Buffered() < length {
					w.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
					w.Flush()
				}
				body = make([]byte, length)
				if _, err := io.ReadFull(r, body); err != nil {
					w.Flush()
					return
				}
			}
		}
		if body == nil {
			body = []byte{}
		}
		rawPath, query := target, ""
		if q := strings.IndexByte(target, '?'); q >= 0 {
			rawPath, query = target[:q], target[q+1:]
		}
		if strings.HasPrefix(rawPath, "http://") || strings.HasPrefix(rawPath, "https://") {
			from := strings.Index(rawPath, "//") + 2
			if slash := strings.IndexByte(rawPath[from:], '/'); slash < 0 {
				rawPath = "/"
			} else {
				rawPath = rawPath[from+slash:]
			}
		}
		connection := strings.ToLower(headers["connection"])
		var close bool
		if version == "HTTP/1.0" {
			close = !strings.Contains(connection, "keep-alive")
		} else {
			close = strings.Contains(connection, "close")
		}
		req := &Request{Method: method, Path: decodePath(rawPath), Query: query, Headers: headers, Body: body, Host: headers["host"]}
		resp := runHandler(handler, lock, req)
		writeResponse(w, resp, method == "HEAD", close)
		if close {
			w.Flush()
			return
		}
	}
}

func runHandler(handler Handler, lock *sync.Mutex, req *Request) (resp *Response) {
	lock.Lock()
	defer lock.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[forge] handler error on %s %s: %v\n%s\n", req.Method, req.Path, rec, debug.Stack())
			resp = errorBody(500, "Internal Server Error")
		}
	}()
	resp = handler(req)
	if resp == nil {
		resp = errorBody(500, "Internal Server Error")
	}
	return resp
}

func serve(ln net.Listener, handler Handler) error {
	var lock sync.Mutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go serveConn(conn, handler, &lock)
	}
}

var _ = bytes.MinRead
