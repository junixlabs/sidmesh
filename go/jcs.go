package sidmesh

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// JCS encodes v per RFC 8785: object keys sorted by UTF-16 code units, no whitespace,
// numbers as ECMAScript prints them, strings with only the escapes JSON requires.
func JCS(v any) ([]byte, error) {
	switch v.(type) {
	case nil, bool, string, float64, map[string]any, []any:
	default: // Go values (ints, structs, typed maps): through their JSON form
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	err := writeJCS(&b, v)
	return b.Bytes(), err
}

func writeJCS(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case float64:
		b.WriteString(jcsNumber(x))
	case string:
		writeString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		type key struct {
			s     string
			units []uint16
		}
		keys := make([]key, 0, len(x))
		for k := range x {
			keys = append(keys, key{k, utf16.Encode([]rune(k))})
		}
		slices.SortFunc(keys, func(a, b key) int { return slices.Compare(a.units, b.units) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k.s)
			b.WriteByte(':')
			if err := writeJCS(b, x[k.s]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		raw, err := JCS(x)
		if err != nil {
			return err
		}
		b.Write(raw)
	}
	return nil
}

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jcsNumber prints f as ECMAScript's Number.prototype.toString does.
func jcsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // e.g. 1.5e+22, 1e-07
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[0]
	exp = strings.TrimLeft(exp[1:], "0")
	return mant + "e" + string(sign) + exp
}
