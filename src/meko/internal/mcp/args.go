package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// args are a tool call's arguments, read as loosely as Meko Cloud reads them: a number may come as
// a string ("10"), a boolean as "true", and an argument no tool knows is ignored. A value that
// can't be read as its type is an argError, which the call reports as a failed call (isError).
type args map[string]json.RawMessage

// argError is an argument of the wrong type.
type argError struct{ name, want string }

func (e *argError) Error() string { return fmt.Sprintf("argument %q must be %s", e.name, e.want) }

func parseArgs(raw json.RawMessage) (args, error) {
	a := args{}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return a, nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%w: %w", errArgsNotObject, err)
	}
	return a, nil
}

// present reports whether an argument was given and isn't null.
func (a args) present(name string) bool {
	v, ok := a[name]
	return ok && string(bytes.TrimSpace(v)) != "null"
}

// str is a string argument ("" when absent or null). A number or boolean is read as its text.
func (a args) str(name string) (string, error) {
	if !a.present(name) {
		return "", nil
	}
	v := a[name]
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err == nil {
		return n.String(), nil
	}
	var b bool
	if err := json.Unmarshal(v, &b); err == nil {
		return strconv.FormatBool(b), nil
	}
	return "", &argError{name, "a string"}
}

// optStr is a string argument, or nil when absent or null.
func (a args) optStr(name string) (*string, error) {
	if !a.present(name) {
		return nil, nil
	}
	s, err := a.str(name)
	return &s, err
}

// integer is an integer argument, or def when absent or null.
func (a args) integer(name string, def int) (int, error) {
	if !a.present(name) {
		return def, nil
	}
	v := a[name]
	var f float64
	if err := json.Unmarshal(v, &f); err == nil && f == float64(int(f)) {
		return int(f), nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n, nil
		}
	}
	return 0, &argError{name, "an integer"}
}

// boolean is a boolean argument, false when absent or null.
func (a args) boolean(name string) (bool, error) {
	b, err := a.optBool(name)
	if err != nil || b == nil {
		return false, err
	}
	return *b, nil
}

// optBool is a boolean argument, or nil when absent or null.
func (a args) optBool(name string) (*bool, error) {
	if !a.present(name) {
		return nil, nil
	}
	v := a[name]
	var b bool
	if err := json.Unmarshal(v, &b); err == nil {
		return &b, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
			return &b, nil
		}
	}
	return nil, &argError{name, "a boolean"}
}

// strings is a list of strings, given as a JSON array (or a JSON string holding one, as some
// clients send).
func (a args) strings(name string) ([]string, error) {
	if !a.present(name) {
		return nil, nil
	}
	v := a[name]
	var list []string
	if err := json.Unmarshal(v, &list); err == nil {
		return list, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if err := json.Unmarshal([]byte(s), &list); err == nil {
			return list, nil
		}
		return []string{s}, nil
	}
	return nil, &argError{name, "a list of strings"}
}

// metadata is the metadata argument, which Meko Cloud takes as a JSON string holding an object,
// also read when a client sends the object itself. ok is false for a string that isn't a JSON
// object.
func (a args) metadata() (obj map[string]any, ok bool, err error) {
	const name = "metadata"
	if !a.present(name) {
		return nil, true, nil
	}
	v := a[name]
	if err := json.Unmarshal(v, &obj); err == nil {
		return obj, true, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, false, &argError{name, "a JSON string"}
	}
	if strings.TrimSpace(s) == "" {
		return nil, true, nil
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, false, nil
	}
	return obj, true, nil
}

// jsonValue unmarshals an argument Meko Cloud takes as a JSON string (or the value itself) into v.
// ok is false for a string that doesn't hold that kind of value.
func (a args) jsonValue(name string, v any) (present, ok bool) {
	if !a.present(name) {
		return false, true
	}
	raw := a[name]
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return false, true
		}
		raw = []byte(s)
	}
	return true, json.Unmarshal(raw, v) == nil
}

// jsonString is s as a JSON string value.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s) //nolint:errchkjson // a string always encodes
	return b
}
