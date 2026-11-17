package interp

import (
	"encoding/json"
	"strconv"
	"strings"
)

var fastJSONCEnabled = true

// WithFastJSONC enables the C fastjson decoder for benchmarking.
func WithFastJSONC() func() {
	return func() { fastJSONCEnabled = true }
}

// fastDecodeObject decodes a JSON object into the interpreter's value model
// (int64 for integral numbers, float64 otherwise) without the allocations of
// encoding/json's decoder, which dominated bulk scans. It reports false for
// anything it does not handle exactly, and the caller falls back to
// encoding/json.
func fastDecodeObject(s string) (map[string]any, bool) {
	if fastJSONCEnabled {
		if m, ok := fastDecodeObjectC(s); ok {
			return m, true
		}
	}
	d := &fastDecoder{s: s}
	d.ws()
	m, ok := d.object()
	if !ok {
		return nil, false
	}
	d.ws()
	return m, d.i == len(d.s)
}

// maxFastDepth is the nesting the fast decoder follows; anything deeper is left
// to encoding/json, which has its own depth limit. Stored properties are flat
// (nested collections cannot be written), so only a damaged file gets near it.
const maxFastDepth = 64

type fastDecoder struct {
	s     string
	i     int
	depth int
}

func (d *fastDecoder) ws() {
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *fastDecoder) object() (map[string]any, bool) {
	if d.i >= len(d.s) || d.s[d.i] != '{' {
		return nil, false
	}
	d.i++
	m := make(map[string]any, 4)
	d.ws()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		return m, true
	}
	for {
		d.ws()
		k, ok := d.str()
		if !ok {
			return nil, false
		}
		d.ws()
		if d.i >= len(d.s) || d.s[d.i] != ':' {
			return nil, false
		}
		d.i++
		v, ok := d.value()
		if !ok {
			return nil, false
		}
		m[k] = v
		d.ws()
		if d.i >= len(d.s) {
			return nil, false
		}
		switch d.s[d.i] {
		case ',':
			d.i++
		case '}':
			d.i++
			return m, true
		default:
			return nil, false
		}
	}
}

func (d *fastDecoder) array() ([]any, bool) {
	d.i++ // '['
	out := []any{}
	d.ws()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		return out, true
	}
	for {
		v, ok := d.value()
		if !ok {
			return nil, false
		}
		out = append(out, v)
		d.ws()
		if d.i >= len(d.s) {
			return nil, false
		}
		switch d.s[d.i] {
		case ',':
			d.i++
		case ']':
			d.i++
			return out, true
		default:
			return nil, false
		}
	}
}

func (d *fastDecoder) value() (any, bool) {
	d.ws()
	if d.i >= len(d.s) {
		return nil, false
	}
	switch c := d.s[d.i]; {
	case c == '{':
		if d.depth++; d.depth > maxFastDepth {
			return nil, false
		}
		m, ok := d.object()
		d.depth--
		return m, ok
	case c == '[':
		if d.depth++; d.depth > maxFastDepth {
			return nil, false
		}
		a, ok := d.array()
		d.depth--
		return a, ok
	case c == '"':
		str, ok := d.str()
		return str, ok
	case c == 't' && strings.HasPrefix(d.s[d.i:], "true"):
		d.i += 4
		return true, true
	case c == 'f' && strings.HasPrefix(d.s[d.i:], "false"):
		d.i += 5
		return false, true
	case c == 'n' && strings.HasPrefix(d.s[d.i:], "null"):
		d.i += 4
		return nil, true
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, false
}

func (d *fastDecoder) number() (any, bool) {
	start := d.i
	float := false
	for d.i < len(d.s) {
		switch c := d.s[d.i]; {
		case c >= '0' && c <= '9' || c == '-' || c == '+':
		case c == '.' || c == 'e' || c == 'E':
			float = true
		default:
			goto done
		}
		d.i++
	}
done:
	lit := d.s[start:d.i]
	if !float {
		if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return n, true
		}
	}
	f, err := strconv.ParseFloat(lit, 64)
	return f, err == nil
}

// str decodes a JSON string. Strings without escapes are sliced directly;
// others go through encoding/json.
func (d *fastDecoder) str() (string, bool) {
	if d.i >= len(d.s) || d.s[d.i] != '"' {
		return "", false
	}
	start := d.i
	d.i++
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case '"':
			d.i++
			if !strings.ContainsRune(d.s[start:d.i], '\\') {
				return d.s[start+1 : d.i-1], true
			}
			var out string
			err := json.Unmarshal([]byte(d.s[start:d.i]), &out)
			return out, err == nil
		case '\\':
			d.i += 2
		default:
			d.i++
		}
	}
	return "", false
}
