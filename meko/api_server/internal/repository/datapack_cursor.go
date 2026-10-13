package repository

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidDatapackCursor is returned for a cursor that is malformed, from
// another version, or was issued for a different sort or filter set.
//
// Unlike DecodeActivityLogCursor this never falls back to the first page: a
// client paging to the end (the ycp agent) that is silently sent back to page
// one would loop or process rows twice.
var ErrInvalidDatapackCursor = errors.New("invalid cursor")

const datapackCursorVersion = 1

type datapackCursorWire struct {
	S string    `json:"s"`
	F string    `json:"f"`
	K [2]string `json:"k"`
	V int       `json:"v"`
}

// DatapackFilterHash identifies a filter set, so a cursor cannot be replayed
// against different filters.
func DatapackFilterHash(q, name, grant string) string {
	sum := sha256.Sum256([]byte(q + "\x00" + name + "\x00" + grant))
	return hex.EncodeToString(sum[:8])
}

// EncodeDatapackCursor returns the opaque wire form of key for a page sorted
// by sortKey with filters filterHash.
func EncodeDatapackCursor(sortKey, filterHash string, key DatapackCursorKey) string {
	w := datapackCursorWire{V: datapackCursorVersion, S: sortKey, F: filterHash}
	if sortKey == DatapackSortName {
		w.K = [2]string{key.Name, key.ID}
	} else {
		w.K = [2]string{key.CreatedAt.UTC().Format(time.RFC3339Nano), key.ID}
	}
	b, err := json.Marshal(w)
	if err != nil {
		// A fixed shape of strings cannot fail to marshal.
		panic(fmt.Sprintf("marshal datapack cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeDatapackCursor parses a cursor and checks it was issued for this sort
// and filter set. Any mismatch is ErrInvalidDatapackCursor.
func DecodeDatapackCursor(s, sortKey, filterHash string) (*DatapackCursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, ErrInvalidDatapackCursor
	}
	var w datapackCursorWire
	if json.Unmarshal(raw, &w) != nil {
		return nil, ErrInvalidDatapackCursor
	}
	// SQL compares the id with ::uuid, which ignores case. The owned name page
	// drops the tie row with a Go string compare, which does not, so an
	// uppercase id in the cursor would skip or repeat that row.
	id := strings.ToLower(w.K[1])
	if w.V != datapackCursorVersion || w.S != sortKey || w.F != filterHash || !isUUID(id) {
		return nil, ErrInvalidDatapackCursor
	}
	key := &DatapackCursorKey{ID: id}
	if sortKey == DatapackSortName {
		key.Name = w.K[0]
		return key, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, w.K[0])
	if err != nil {
		return nil, ErrInvalidDatapackCursor
	}
	key.CreatedAt = ts
	return key, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
