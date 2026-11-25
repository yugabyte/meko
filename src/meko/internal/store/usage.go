package store

import (
	"context"
	"fmt"
)

// ContentUsage is how many bytes of content Meko holds, by kind, as content_usage's triggers keep
// it (0014_content_usage.sql): what the storage limit counts.
type ContentUsage struct {
	Memories      int64 `json:"memories"`      // memory text and payload, promoted memories' too
	Conversations int64 `json:"conversations"` // turns: input, output, reasoning, plan and metadata
	Knowledge     int64 `json:"knowledge"`     // knowledge files' bytes
	Artifacts     int64 `json:"artifacts"`     // artifacts' bytes
}

// Total is every kind's bytes together.
func (u ContentUsage) Total() int64 { return u.Memories + u.Conversations + u.Knowledge + u.Artifacts }

// ContentUsage reads how much content Meko holds: four rows, whatever the size of the data.
func (s *Store) ContentUsage(ctx context.Context) (ContentUsage, error) {
	var u ContentUsage
	rows, err := s.r.QueryContext(ctx, "SELECT kind, bytes FROM content_usage")
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int64
		if err = rows.Scan(&kind, &n); err != nil {
			return u, err
		}
		switch kind {
		case "memories":
			u.Memories = n
		case "conversations":
			u.Conversations = n
		case "knowledge":
			u.Knowledge = n
		case "artifacts":
			u.Artifacts = n
		default:
			return u, fmt.Errorf("content_usage has an unknown kind %q", kind)
		}
	}
	return u, rows.Err()
}
