package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// blankJSONC is a JSON or JSONC document with its comments and trailing commas turned into spaces
// (newlines kept), so the same bytes are at the same offsets: a strict JSON reader reads it, and an
// offset it gives points into the original too.
func blankJSONC(b []byte) []byte { return blankTrailingCommas(blankComments(b)) }

// blankComments turns a JSONC document's comments into spaces, keeping their newlines.
func blankComments(b []byte) []byte {
	out := slices.Clone(b)
	blank := func(i, j int) {
		for ; i < j; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == '"':
			i = stringEnd(out, i)
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			j := bytes.IndexByte(out[i:], '\n')
			if j < 0 {
				j = len(out) - i
			}
			blank(i, i+j)
			i += j
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '*':
			j := bytes.Index(out[i+2:], []byte("*/"))
			end := len(out)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			blank(i, end)
			i = end - 1
		}
	}
	return out
}

// blankTrailingCommas turns the commas before a closing } or ] into spaces, in a document without
// comments.
func blankTrailingCommas(b []byte) []byte {
	out := slices.Clone(b)
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '"':
			i = stringEnd(out, i)
		case ',':
			j := i + 1
			for j < len(out) && isSpace(out[j]) {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}
	return out
}

// stringEnd is the offset of the quote closing the string that opens at i.
func stringEnd(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(b)
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// removeMember takes the meko member out of the object at path (keys from the top) in a JSON or
// JSONC document, as text: the rest of the document, its layout and comments, stays as it was. ok
// is false when there's no such member.
func removeMember(doc []byte, path []string) (out []byte, ok bool, err error) {
	const key = ServerName
	// nc has the commas a trailing one included; clean is what a strict reader reads.
	nc := blankComments(doc)
	clean := blankTrailingCommas(nc)
	sp, err := locate(clean, path, key)
	if err != nil || !sp.member {
		return doc, false, err
	}
	start, end := sp.start, sp.end
	j := end
	for j < len(nc) && isSpace(nc[j]) {
		j++
	}
	if j < len(nc) && nc[j] == ',' {
		// A comma after it (before another member, or a trailing one): it goes, with the spaces after.
		end = j + 1
		for end < len(nc) && (nc[end] == ' ' || nc[end] == '\t') {
			end++
		}
	} else {
		// The last member: the comma before it goes.
		i := start - 1
		for i >= 0 && isSpace(nc[i]) {
			i--
		}
		if i >= 0 && nc[i] == ',' {
			start = i
		}
	}
	// A member on a line of its own takes the line with it.
	ls := start
	for ls > 0 && (nc[ls-1] == ' ' || nc[ls-1] == '\t') {
		ls--
	}
	le := end
	for le < len(nc) && (nc[le] == ' ' || nc[le] == '\t' || nc[le] == '\r') {
		le++
	}
	if (ls == 0 || nc[ls-1] == '\n') && le < len(nc) && nc[le] == '\n' {
		start, end = ls, le+1
	}
	out = append(slices.Clone(doc[:start]), doc[end:]...)
	if after, ferr := locate(blankJSONC(out), path, key); ferr != nil || after.member {
		return doc, false, errors.New("the meko server couldn't be taken out")
	}
	return out, true, nil
}

// spot is where locate found things in a document: the object at a path (from its { to its }),
// and in it a member (from its key's opening quote to the end of its value, which starts at val).
type spot struct {
	objOpen, objClose int
	obj               bool
	start, val, end   int
	member            bool
}

// locate finds the object at path in a strict JSON document, and its member key.
func locate(clean []byte, path []string, key string) (spot, error) {
	var sp spot
	dec := json.NewDecoder(bytes.NewReader(clean))
	// value reads one value; at is the path to it while it's on the way to path, else nil.
	var value func(at []string) error
	value = func(at []string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, isDelim := tok.(json.Delim)
		if !isDelim {
			return nil
		}
		here := at != nil && len(at) == len(path) && d == '{' && !sp.obj
		if here {
			sp.objOpen = int(dec.InputOffset()) - 1
		}
		for dec.More() {
			if d == '[' {
				if verr := value(nil); verr != nil {
					return verr
				}
				continue
			}
			kt, kerr := dec.Token()
			if kerr != nil {
				return kerr
			}
			k, _ := kt.(string)
			if here && k == key && !sp.member {
				keyEnd := int(dec.InputOffset())
				var raw json.RawMessage
				if derr := dec.Decode(&raw); derr != nil {
					return derr
				}
				sp.start = bytes.LastIndexByte(clean[:keyEnd-1], '"')
				sp.end, sp.member = int(dec.InputOffset()), true
				sp.val = sp.end - len(raw)
				continue
			}
			var next []string
			if at != nil && len(at) < len(path) && path[len(at)] == k {
				next = append(slices.Clone(at), k)
			}
			if verr := value(next); verr != nil {
				return verr
			}
		}
		if _, err = dec.Token(); err != nil { // the closing delimiter
			return err
		}
		if here {
			sp.objClose, sp.obj = int(dec.InputOffset())-1, true
		}
		return nil
	}
	if err := value([]string{}); err != nil {
		return spot{}, err
	}
	return sp, nil
}

// lineIndent is the spaces and tabs that start the line holding offset i.
func lineIndent(b []byte, i int) string {
	ls := bytes.LastIndexByte(b[:i], '\n') + 1
	le := ls
	for le < len(b) && (b[le] == ' ' || b[le] == '\t') {
		le++
	}
	return string(b[ls:le])
}

// indented is a compact JSON value laid out two spaces a level, its lines after the first starting
// with prefix.
func indented(compact []byte, prefix string) []byte {
	var buf bytes.Buffer
	if json.Indent(&buf, compact, prefix, "  ") != nil {
		return compact
	}
	return buf.Bytes()
}

// setMember sets the member key of the object at path in a JSON or JSONC document to value (a
// compact JSON value), as text: a member there is replaced, or one is added at the end of the object,
// which is added too when path's last object is missing (one level, in the object above it). The
// rest of the document stays as it was. A document that's empty or null becomes a new one.
func setMember(doc []byte, path []string, key string, value []byte) ([]byte, error) {
	if t := bytes.TrimSpace(blankComments(doc)); len(t) == 0 || string(t) == "null" {
		v := value
		keys := append(slices.Clone(path), key)
		for i := len(keys) - 1; i >= 0; i-- {
			kb, _ := json.Marshal(keys[i])
			v = slices.Concat([]byte("{"), kb, []byte(":"), v, []byte("}"))
		}
		return append(indented(v, ""), '\n'), nil
	}
	clean := blankJSONC(doc)
	sp, err := locate(clean, path, key)
	if err != nil {
		return nil, err
	}
	switch {
	case sp.member:
		v := indented(value, lineIndent(clean, sp.start))
		return slices.Concat(doc[:sp.val], v, doc[sp.end:]), nil
	case sp.obj:
		return insertMember(doc, clean, sp, key, value), nil
	case len(path) > 0:
		kb, _ := json.Marshal(key)
		inner := slices.Concat([]byte("{"), kb, []byte(":"), value, []byte("}"))
		up, err := locate(clean, path[:len(path)-1], path[len(path)-1])
		if err != nil {
			return nil, err
		}
		if !up.obj || up.member { // the path holds something other than an object there
			return nil, errors.New("its " + path[len(path)-1] + " isn't an object")
		}
		return insertMember(doc, clean, up, path[len(path)-1], inner), nil
	}
	return nil, errors.New("it isn't a JSON object")
}

// insertMember adds key: value at the end of the object sp found: on a line of its own, indented as
// the object's first member, when the object spans lines.
func insertMember(doc, clean []byte, sp spot, key string, value []byte) []byte {
	kb, _ := json.Marshal(key)
	last := sp.objClose - 1
	for last > sp.objOpen && isSpace(clean[last]) {
		last--
	}
	multiline := bytes.IndexByte(clean[sp.objOpen:sp.objClose], '\n') >= 0
	if last == sp.objOpen { // an empty object
		if !multiline {
			return slices.Concat(doc[:sp.objOpen+1], kb, []byte(": "), value, doc[sp.objClose:])
		}
		in := lineIndent(clean, sp.objOpen)
		member := slices.Concat([]byte("\n"+in+"  "), kb, []byte(": "), indented(value, in+"  "), []byte("\n"+in))
		return slices.Concat(doc[:sp.objOpen+1], member, doc[sp.objClose:])
	}
	if !multiline {
		return slices.Concat(doc[:last+1], []byte(", "), kb, []byte(": "), value, doc[last+1:])
	}
	first := sp.objOpen + 1
	for first < sp.objClose && isSpace(clean[first]) {
		first++
	}
	in := lineIndent(clean, first)
	// After a trailing comma when there's one, else after a comma of its own.
	at, comma := last+1, []byte(",")
	nc := blankComments(doc)
	j := at
	for j < sp.objClose && isSpace(nc[j]) {
		j++
	}
	if nc[j] == ',' {
		at, comma = j+1, nil
	}
	member := slices.Concat(comma, []byte("\n"+in), kb, []byte(": "), indented(value, in))
	return slices.Concat(doc[:at], member, doc[at:])
}

// members are a JSON object's members in their order; nil when raw isn't an object.
func members(raw []byte) [][2][]byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	out := [][2][]byte{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		kb, _ := json.Marshal(kt)
		out = append(out, [2][]byte{kb, v})
	}
	return out
}

// withMembers is raw (an object, else an empty one) with each of set's members replaced or added at
// the end, and drop's taken out, keeping the rest in their order, compact.
func withMembers(raw []byte, set [][2]string, drop ...string) []byte {
	ms := members(raw)
	for _, kv := range set {
		kb, _ := json.Marshal(kv[0])
		found := false
		for i := range ms {
			if bytes.Equal(ms[i][0], kb) {
				ms[i][1], found = []byte(kv[1]), true
			}
		}
		if !found {
			ms = append(ms, [2][]byte{kb, []byte(kv[1])})
		}
	}
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	for _, m := range ms {
		var k string
		_ = json.Unmarshal(m[0], &k)
		if slices.Contains(drop, k) {
			continue
		}
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		b.Write(m[0])
		b.WriteByte(':')
		_ = json.Compact(&b, m[1])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString is s as a JSON string, without HTML escapes.
func jsonString(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return strings.TrimSpace(b.String())
}
