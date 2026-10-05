package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type obj = map[string]any

func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func asObj(v any) obj {
	if m, ok := v.(obj); ok {
		return m
	}
	return obj{}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case []any:
		return len(x) > 0
	case obj:
		return len(x) > 0
	}
	return true
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return compact(v)
}

func intOf(v any) int {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		f, _ := x.Float64()
		return int(f)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(x))
		return i
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

func isInt(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	_, err := n.Int64()
	return err == nil
}

func compact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func head(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func approxTokens(text string) int { return max(1, len(strings.Fields(text))) }

func splitLines(s string) []string {
	var out []string
	rs := []rune(s)
	cur := make([]rune, 0, 64)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			out = append(out, string(cur))
			cur = cur[:0]
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, rs[i])
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// spacedJSON renders v with ", " and ": " separators, sorted keys and ASCII
// escapes, for JSON that is embedded in text fields the stack parses or compares.
func spacedJSON(v any) string {
	var b strings.Builder
	spacedEncode(&b, v)
	return b.String()
}

func spacedEncode(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		spacedString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			spacedEncode(b, e)
		}
		b.WriteByte(']')
	case obj:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			spacedString(b, k)
			b.WriteString(": ")
			spacedEncode(b, x[k])
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			spacedString(b, fmt.Sprint(v))
			return
		}
		back, err := decode(raw)
		if err != nil {
			spacedString(b, fmt.Sprint(v))
			return
		}
		spacedEncode(b, back)
	}
}

func spacedString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r >= 0x7f && r < 0x10000):
			if r == 0x7f {
				b.WriteRune(r)
				continue
			}
			fmt.Fprintf(b, `\u%04x`, r)
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
