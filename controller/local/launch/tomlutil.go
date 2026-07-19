package launch

import (
	"bytes"

	"github.com/pelletier/go-toml/v2"
)

// unmarshalTOMLMap parses a TOML document into a generic map (used to
// read-modify-write a pageserver.toml when cloning it for a branch).
func unmarshalTOMLMap(data []byte) (map[string]any, error) {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// marshalTOMLMap serializes a generic map back to TOML.
func marshalTOMLMap(m map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
