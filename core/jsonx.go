// Ordered JSON values (insertion-ordered objects, as JavaScript builds them) and a writer that formats
// like JSON.stringify, plus the "undefined" marker for absent input fields.
package main

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

type undefT struct{}

// Undefined marks an absent field (JavaScript's undefined, as opposed to null).
var Undefined any = undefT{}

func isUndef(v any) bool { _, ok := v.(undefT); return ok }

// field reads a key of a parsed JSON object, Undefined when absent or when m is not an object.
func field(m any, key string) any {
	obj, ok := m.(map[string]any)
	if !ok {
		return Undefined
	}
	if v, ok := obj[key]; ok {
		return v
	}
	return Undefined
}

// Obj is an insertion-ordered JSON object.
type Obj struct {
	k []string
	v []any
}

// O builds an object from alternating keys and values.
func O(kv ...any) *Obj {
	o := &Obj{k: make([]string, 0, len(kv)/2), v: make([]any, 0, len(kv)/2)}
	for i := 0; i+1 < len(kv); i += 2 {
		o.k = append(o.k, kv[i].(string))
		o.v = append(o.v, kv[i+1])
	}
	return o
}

func (o *Obj) Set(key string, val any) *Obj {
	for i, k := range o.k {
		if k == key {
			o.v[i] = val
			return o
		}
	}
	o.k = append(o.k, key)
	o.v = append(o.v, val)
	return o
}

func (o *Obj) Get(key string) (any, bool) {
	for i, k := range o.k {
		if k == key {
			return o.v[i], true
		}
	}
	return nil, false
}

func (o *Obj) Delete(key string) {
	for i, k := range o.k {
		if k == key {
			o.k = append(o.k[:i:i], o.k[i+1:]...)
			o.v = append(o.v[:i:i], o.v[i+1:]...)
			return
		}
	}
}

// Raw is pre-encoded JSON embedded as is.
type Raw []byte

func (o *Obj) MarshalJSON() ([]byte, error) { return appendJSON(nil, o), nil }

func toJSON(v any) []byte { return appendJSON(make([]byte, 0, 256), v) }

func toJSONString(v any) string { return string(toJSON(v)) }

func appendJSON(dst []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...)
	case undefT:
		return append(dst, "null"...)
	case string:
		return appendJSONString(dst, x)
	case *string:
		if x == nil {
			return append(dst, "null"...)
		}
		return appendJSONString(dst, *x)
	case bool:
		if x {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case int64:
		return strconv.AppendInt(dst, x, 10)
	case float64:
		return append(dst, jsNumber(x)...)
	case json.Number:
		return append(dst, x...)
	case Raw:
		return append(dst, x...)
	case *Obj:
		if x == nil {
			return append(dst, "null"...)
		}
		dst = append(dst, '{')
		first := true
		for i, k := range x.k {
			if isUndef(x.v[i]) {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSON(dst, x.v[i])
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if isUndef(e) {
				dst = append(dst, "null"...)
			} else {
				dst = appendJSON(dst, e)
			}
		}
		return append(dst, ']')
	case []*Obj:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSON(dst, e)
		}
		return append(dst, ']')
	case []string:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, e)
		}
		return append(dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		first := true
		for _, k := range keys {
			if isUndef(x[k]) {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSON(dst, x[k])
		}
		return append(dst, '}')
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return append(dst, "null"...)
		}
		return append(dst, b...)
	}
}

const hexLower = "0123456789abcdef"

// appendJSONString quotes s as JSON.stringify does (no HTML escaping).
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&15])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, "�"...)
			i++
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// jsNumber formats a float as JavaScript's Number#toString does for the common cases.
func jsNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	// Go writes 1e-07; JavaScript writes 1e-7 and 1e+21.
	for i := 0; i < len(s); i++ {
		if s[i] == 'e' {
			mant, exp := s[:i], s[i+1:]
			sign := exp[0]
			digits := exp[1:]
			for len(digits) > 1 && digits[0] == '0' {
				digits = digits[1:]
			}
			return mant + "e" + string(sign) + digits
		}
	}
	return s
}

// jsString converts a parsed JSON value the way String(value) does in JavaScript.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case undefT:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNumber(x)
	case json.Number:
		return string(x)
	case []any:
		out := ""
		for i, e := range x {
			if i > 0 {
				out += ","
			}
			if e != nil && !isUndef(e) {
				out += jsString(e)
			}
		}
		return out
	default:
		return "[object Object]"
	}
}

// parseJSON parses a request body into generic values (objects as map[string]any, numbers as float64).
func parseJSON(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// orderedErr turns an error detail map into an ordered object with GitHub's usual key order.
func orderedErr(m map[string]any) *Obj {
	o := &Obj{}
	for _, k := range []string{"value", "resource", "field", "code", "message"} {
		if v, ok := m[k]; ok {
			o.Set(k, v)
		}
	}
	keys := make([]string, 0)
	for k := range m {
		if _, ok := o.Get(k); !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}
