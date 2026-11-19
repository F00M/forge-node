// URLSearchParams semantics (ordered pairs, first-match get, set replaces in place) and Node's lenient
// base64 decoding, both used where the JavaScript forge relied on them.
package main

import (
	"encoding/base64"
	"net/url"
	"strings"
)

type Params struct {
	keys []string
	vals []string
}

func parseParams(query string) *Params {
	p := &Params{}
	if query == "" {
		return p
	}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		p.keys = append(p.keys, formDecode(k))
		p.vals = append(p.vals, formDecode(v))
	}
	return p
}

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	if out, err := url.PathUnescape(s); err == nil {
		return out
	}
	// Lenient: decode valid %XX escapes, keep the rest.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// Get returns the first value for name and whether it was present.
func (p *Params) Get(name string) (string, bool) {
	for i, k := range p.keys {
		if k == name {
			return p.vals[i], true
		}
	}
	return "", false
}

func (p *Params) GetOr(name, fallback string) string {
	if v, ok := p.Get(name); ok {
		return v
	}
	return fallback
}

func (p *Params) Has(name string) bool { _, ok := p.Get(name); return ok }

func (p *Params) Clone() *Params {
	return &Params{keys: append([]string(nil), p.keys...), vals: append([]string(nil), p.vals...)}
}

// Set replaces the first pair named name (dropping the others) or appends one.
func (p *Params) Set(name, value string) {
	found := false
	keys, vals := p.keys[:0:0], p.vals[:0:0]
	for i, k := range p.keys {
		if k == name {
			if found {
				continue
			}
			found = true
			keys = append(keys, k)
			vals = append(vals, value)
			continue
		}
		keys = append(keys, k)
		vals = append(vals, p.vals[i])
	}
	if !found {
		keys = append(keys, name)
		vals = append(vals, value)
	}
	p.keys, p.vals = keys, vals
}

func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

func (p *Params) String() string {
	parts := make([]string, len(p.keys))
	for i, k := range p.keys {
		parts[i] = formEncode(k) + "=" + formEncode(p.vals[i])
	}
	return strings.Join(parts, "&")
}

// nodeBase64Decode decodes like Buffer.from(text, 'base64'): whitespace and stray characters are skipped,
// both alphabets are accepted, padding is optional and the first '=' ends the data.
func nodeBase64Decode(text string) []byte {
	clean := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '=':
			i = len(text)
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/':
			clean = append(clean, c)
		case c == '-':
			clean = append(clean, '+')
		case c == '_':
			clean = append(clean, '/')
		}
	}
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out, err := base64.RawStdEncoding.DecodeString(string(clean))
	if err != nil {
		return []byte{}
	}
	return out
}
