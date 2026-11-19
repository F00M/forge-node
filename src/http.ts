// A small HTTP/1.1 server on node:net: keep-alive, pipelining, Content-Length and chunked request bodies,
// Expect: 100-continue. Handlers are synchronous and answer with one write, so responses stay in order.

import { createServer, type Server, type Socket } from 'node:net';

export interface Request {
  method: string;
  /** The decoded path, without the query string. */
  path: string;
  /** The raw query string (no leading '?'). */
  query: string;
  /** Lower-cased header names; repeated headers are joined with ', '. */
  headers: Record<string, string>;
  body: Buffer;
}

export interface Response {
  status: number;
  /** Content-Type header value. */
  type: string;
  body: string | Buffer;
  /** Extra header lines, each already formatted as `Name: value\r\n`. */
  extra?: string;
}

export type Handler = (request: Request) => Response;

const REASONS: Record<number, string> = {
  100: 'Continue',
  200: 'OK',
  201: 'Created',
  202: 'Accepted',
  204: 'No Content',
  301: 'Moved Permanently',
  304: 'Not Modified',
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  411: 'Length Required',
  413: 'Payload Too Large',
  415: 'Unsupported Media Type',
  422: 'Unprocessable Entity',
  431: 'Request Header Fields Too Large',
  500: 'Internal Server Error',
  501: 'Not Implemented',
};

const MAX_HEADER_BYTES = 64 * 1024;
const MAX_BODY_BYTES = 512 * 1024 * 1024;
const EMPTY = Buffer.alloc(0);

let requestCounter = 0x5a3c00;

function requestId(): string {
  requestCounter += 1;
  return `C0DE:${requestCounter.toString(16).toUpperCase()}:F0RGE`;
}

function decodePath(raw: string): string {
  if (raw.indexOf('%') < 0) return raw;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** Serialize one response into the bytes (or string) to write. */
function serialize(response: Response, head: boolean, close: boolean): string | Buffer {
  const status = response.status;
  const noBody = status === 204 || status === 304 || head;
  const body = response.body;
  const length = typeof body === 'string' ? Buffer.byteLength(body) : body.length;
  let header = `HTTP/1.1 ${status} ${REASONS[status] ?? 'Unknown'}\r\nContent-Type: ${response.type}\r\nX-GitHub-Request-Id: ${requestId()}\r\n`;
  if (status !== 204 && status !== 304) header += `Content-Length: ${length}\r\n`;
  if (response.extra !== undefined) header += response.extra;
  if (close) header += 'Connection: close\r\n';
  header += '\r\n';
  if (noBody || length === 0) return header;
  if (typeof body === 'string') return header + body;
  return Buffer.concat([Buffer.from(header, 'latin1'), body]);
}

interface Parsed {
  request: Request;
  close: boolean;
  head: boolean;
  /** Bytes consumed from the connection buffer. */
  consumed: number;
}

/** Decode a chunked body starting at `start`; null when more bytes are needed. */
function decodeChunked(buf: Buffer, start: number): { body: Buffer; end: number } | null {
  const parts: Buffer[] = [];
  let p = start;
  let total = 0;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', p, 'latin1');
    if (lineEnd < 0) return null;
    const sizeText = buf.toString('latin1', p, lineEnd).split(';')[0]!.trim();
    const size = Number.parseInt(sizeText, 16);
    if (!Number.isFinite(size) || size < 0) throw new Error('bad chunk size');
    p = lineEnd + 2;
    if (size === 0) {
      // Trailers: skip header lines until an empty line.
      for (;;) {
        const end = buf.indexOf('\r\n', p, 'latin1');
        if (end < 0) return null;
        if (end === p) return { body: parts.length === 1 ? parts[0]! : Buffer.concat(parts, total), end: end + 2 };
        p = end + 2;
      }
    }
    if (p + size + 2 > buf.length) return null;
    parts.push(buf.subarray(p, p + size));
    total += size;
    if (total > MAX_BODY_BYTES) throw new Error('body too large');
    p += size + 2;
  }
}

class ParseError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * Parse one request from the front of `buf`; null when incomplete, with `state.need` set to the byte count
 * that must be buffered before trying again. Answers Expect: 100-continue once per request.
 */
function parseOne(buf: Buffer, state: { continued: boolean; socket: Socket; need: number }): Parsed | null {
  const headerEnd = buf.indexOf('\r\n\r\n', 0, 'latin1');
  if (headerEnd < 0) {
    if (buf.length > MAX_HEADER_BYTES) throw new ParseError(431, 'headers too large');
    state.need = buf.length + 1;
    return null;
  }
  const headText = buf.toString('latin1', 0, headerEnd);
  const lines = headText.split('\r\n');
  const requestLine = lines[0]!;
  const firstSpace = requestLine.indexOf(' ');
  const lastSpace = requestLine.lastIndexOf(' ');
  if (firstSpace <= 0 || lastSpace <= firstSpace) throw new ParseError(400, 'bad request line');
  const method = requestLine.slice(0, firstSpace).toUpperCase();
  const target = requestLine.slice(firstSpace + 1, lastSpace);
  const version = requestLine.slice(lastSpace + 1);
  const headers: Record<string, string> = {};
  for (let i = 1; i < lines.length; i += 1) {
    const line = lines[i]!;
    const colon = line.indexOf(':');
    if (colon <= 0) continue;
    const name = line.slice(0, colon).trim().toLowerCase();
    const value = line.slice(colon + 1).trim();
    const existing = headers[name];
    headers[name] = existing === undefined ? value : `${existing}, ${value}`;
  }
  const bodyStart = headerEnd + 4;
  let body = EMPTY;
  let consumed = bodyStart;
  const transferEncoding = headers['transfer-encoding'];
  if (transferEncoding !== undefined && transferEncoding.toLowerCase().includes('chunked')) {
    if (!state.continued && (headers.expect ?? '').toLowerCase() === '100-continue') {
      state.continued = true;
      state.socket.write('HTTP/1.1 100 Continue\r\n\r\n');
    }
    let decoded: { body: Buffer; end: number } | null;
    try {
      decoded = decodeChunked(buf, bodyStart);
    } catch {
      throw new ParseError(400, 'bad chunked body');
    }
    if (decoded === null) {
      state.need = buf.length + 1;
      return null;
    }
    body = decoded.body;
    consumed = decoded.end;
  } else {
    const lengthText = headers['content-length'];
    if (lengthText !== undefined) {
      const length = Number.parseInt(lengthText, 10);
      if (!Number.isFinite(length) || length < 0) throw new ParseError(400, 'bad content-length');
      if (length > MAX_BODY_BYTES) throw new ParseError(413, 'body too large');
      if (buf.length < bodyStart + length) {
        if (!state.continued && (headers.expect ?? '').toLowerCase() === '100-continue') {
          state.continued = true;
          state.socket.write('HTTP/1.1 100 Continue\r\n\r\n');
        }
        state.need = bodyStart + length;
        return null;
      }
      body = buf.subarray(bodyStart, bodyStart + length);
      consumed = bodyStart + length;
    }
  }
  const question = target.indexOf('?');
  let rawPath = question < 0 ? target : target.slice(0, question);
  if (rawPath.startsWith('http://') || rawPath.startsWith('https://')) {
    const slash = rawPath.indexOf('/', rawPath.indexOf('//') + 2);
    rawPath = slash < 0 ? '/' : rawPath.slice(slash);
  }
  const connection = (headers.connection ?? '').toLowerCase();
  const close = version === 'HTTP/1.0' ? !connection.includes('keep-alive') : connection.includes('close');
  return {
    request: { method, path: decodePath(rawPath), query: question < 0 ? '' : target.slice(question + 1), headers, body },
    close,
    head: method === 'HEAD',
    consumed,
  };
}

function errorBody(status: number, message: string): Response {
  return { status, type: 'application/json; charset=utf-8', body: JSON.stringify({ message }) };
}

export function createHttpServer(handler: Handler): Server {
  const server = createServer({ noDelay: true }, (socket: Socket) => {
    socket.setNoDelay(true);
    const chunks: Buffer[] = [];
    let buffered = 0;
    const state = { continued: false, socket, need: 0 };
    socket.on('error', () => {
      socket.destroy();
    });
    socket.on('data', (chunk: Buffer) => {
      chunks.push(chunk);
      buffered += chunk.length;
      if (buffered < state.need) return;
      let buf: Buffer = chunks.length === 1 ? chunks[0]! : Buffer.concat(chunks, buffered);
      chunks.length = 0;
      buffered = 0;
      state.need = 0;
      let out: Array<string | Buffer> | null = null;
      for (;;) {
        if (buf.length === 0) break;
        let parsed: Parsed | null;
        try {
          parsed = parseOne(buf, state);
        } catch (err) {
          const status = err instanceof ParseError ? err.status : 400;
          const bytes = serialize(errorBody(status, err instanceof Error ? err.message : 'bad request'), false, true);
          if (out !== null) for (const piece of out) socket.write(piece);
          socket.end(bytes);
          return;
        }
        if (parsed === null) {
          chunks.push(buf);
          buffered = buf.length;
          break;
        }
        state.continued = false;
        let response: Response;
        try {
          response = handler(parsed.request);
        } catch (err) {
          process.stderr.write(`[forge] handler error on ${parsed.request.method} ${parsed.request.path}: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}
`);
          response = errorBody(500, 'Internal Server Error');
        }
        const bytes = serialize(response, parsed.head, parsed.close);
        if (out === null) out = [bytes];
        else out.push(bytes);
        if (parsed.close) {
          for (const piece of out) socket.write(piece);
          socket.end();
          return;
        }
        buf = parsed.consumed >= buf.length ? EMPTY : buf.subarray(parsed.consumed);
      }
      if (out !== null) {
        if (out.length === 1) socket.write(out[0]!);
        else {
          socket.cork();
          for (const piece of out) socket.write(piece);
          socket.uncork();
        }
      }
    });
  });
  server.on('error', (err) => {
    process.stderr.write(`[forge] server error: ${err.message}\n`);
  });
  return server;
}
