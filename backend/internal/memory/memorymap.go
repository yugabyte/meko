package memory

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

// mapMemories is the most memories a map draws, newest first: more is a blur, not a map.
const mapMemories = 300

// mapLinks is how many of a memory's nearest others it's linked to, and mapLinkMin the least
// similarity a link needs: a few links each show groups, every pair is a hairball. MiniLM scores
// short related facts 0.3 to 0.5 (a release rule and the release train, a customer and its
// request), so 0.35 joins the closest of them and leaves unrelated ones apart.
const (
	mapLinks   = 3
	mapLinkMin = 0.35
)

// MapPoint is one memory on the map: x and y in [-1, 1], placed so that memories alike in meaning
// sit close. Similarity is its cosine to the query, when there is one.
type MapPoint struct {
	ID             string   `json:"id"`
	Text           string   `json:"text"`
	State          string   `json:"state"` // learning, kept or collective
	AgentID        string   `json:"agent_id"`
	ConversationID string   `json:"conversation_id,omitempty"`
	CreatedAt      string   `json:"created_at"`
	X              float64  `json:"x"`
	Y              float64  `json:"y"`
	Similarity     *float64 `json:"similarity,omitempty"`
}

// MapLink joins two memories alike in meaning, by their places in Points.
type MapLink struct {
	A          int     `json:"a"`
	B          int     `json:"b"`
	Similarity float64 `json:"similarity"`
}

// Map is a datapack's memories laid out by meaning, and where a query lands among them.
type Map struct {
	Points []MapPoint  `json:"points"`
	Links  []MapLink   `json:"links"`
	Query  *[2]float64 `json:"query,omitempty"`
}

// MemoryMap lays out a datapack's newest memories (learnings, kept and collective) by their
// embeddings, projected onto their two main directions (PCA), with each linked to its nearest
// alike. With a query, it's embedded and placed the same way, and each memory gets its similarity to
// it. A memory the sweep hasn't embedded yet is left out.
func (s *Service) MemoryMap(ctx context.Context, datapack, query string) (Map, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return Map{}, err
	}
	var points []MapPoint
	var vecs [][]float32
	err := s.st.Snapshot(ctx, func(st *store.Store) error {
		learnings, err := ids(func(offset int) ([]string, error) {
			recs, _, lerr := st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
				Scope: store.ScopeLearnings, Limit: exportPage, Offset: offset})
			return recordIDs(recs), lerr
		})
		if err != nil {
			return err
		}
		recs, _, err := st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
			Limit: mapMemories})
		if err != nil {
			return err
		}
		for _, r := range recs {
			v, verr := st.MemoryEmbedding(ctx, datapack, r.ID)
			if verr != nil {
				return verr
			}
			if v == nil {
				continue
			}
			m := fromRecord(datapack, r)
			state := "kept"
			if learnings[m.ID] {
				state = "learning"
			}
			points = append(points, MapPoint{ID: m.ID, Text: m.Memory, State: state, AgentID: m.AgentID,
				ConversationID: m.RunID, CreatedAt: m.CreatedAt})
			vecs = append(vecs, v)
		}
		ps, _, err := st.PromotedMemories(ctx, datapack, mapMemories, 0)
		if err != nil {
			return err
		}
		for _, p := range ps {
			v, verr := st.PromotedEmbedding(ctx, datapack, p.ID)
			if verr != nil {
				return verr
			}
			if v == nil {
				continue
			}
			points = append(points, MapPoint{ID: p.ID, Text: p.Text, State: "collective", AgentID: p.AgentID,
				ConversationID: p.ConversationID, CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339)})
			vecs = append(vecs, v)
		}
		return nil
	})
	if err != nil {
		return Map{}, err
	}
	out := Map{Points: points, Links: links(vecs)}
	if out.Points == nil {
		out.Points = []MapPoint{}
	}
	var q []float32
	if query != "" && len(vecs) > 0 {
		qs, qerr := s.eng.Embed(ctx, []string{query})
		if qerr != nil {
			return Map{}, qerr
		}
		q = qs[0]
		for i := range out.Points {
			sim := cosine(q, vecs[i])
			out.Points[i].Similarity = &sim
		}
	}
	xy, qxy := project(vecs, q)
	for i := range out.Points {
		out.Points[i].X, out.Points[i].Y = xy[i][0], xy[i][1]
	}
	if q != nil {
		out.Query = &qxy
	}
	return out, nil
}

// links joins each vector to its mapLinks nearest others at mapLinkMin or above, each pair once.
func links(vecs [][]float32) []MapLink {
	out := []MapLink{}
	seen := map[[2]int]bool{}
	for i := range vecs {
		var near []MapLink
		for j := range vecs {
			if j == i {
				continue
			}
			if sim := cosine(vecs[i], vecs[j]); sim >= mapLinkMin {
				near = append(near, MapLink{A: min(i, j), B: max(i, j), Similarity: sim})
			}
		}
		sort.Slice(near, func(a, b int) bool { return near[a].Similarity > near[b].Similarity })
		for _, l := range near[:min(len(near), mapLinks)] {
			if k := [2]int{l.A, l.B}; !seen[k] {
				seen[k] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// project places vectors (and q, when given) on the plane of their two main directions, found by
// power iteration on the centered vectors, scaled so the farthest point sits at 1.
func project(vecs [][]float32, q []float32) ([][2]float64, [2]float64) {
	out := make([][2]float64, len(vecs))
	if len(vecs) == 0 {
		return out, [2]float64{}
	}
	dims := len(vecs[0])
	mean := make([]float64, dims)
	for _, v := range vecs {
		for d, x := range v {
			mean[d] += float64(x) / float64(len(vecs))
		}
	}
	centered := func(v []float32) []float64 {
		c := make([]float64, dims)
		for d, x := range v {
			c[d] = float64(x) - mean[d]
		}
		return c
	}
	rows := make([][]float64, len(vecs))
	for i, v := range vecs {
		rows[i] = centered(v)
	}
	first := direction(rows, nil)
	second := direction(rows, first)
	at := func(c []float64) [2]float64 { return [2]float64{dot(c, first), dot(c, second)} }
	scale := 0.0
	for i, r := range rows {
		out[i] = at(r)
		scale = math.Max(scale, math.Hypot(out[i][0], out[i][1]))
	}
	var qxy [2]float64
	if q != nil {
		qxy = at(centered(q))
		scale = math.Max(scale, math.Hypot(qxy[0], qxy[1]))
	}
	if scale == 0 {
		return out, qxy
	}
	for i := range out {
		out[i][0], out[i][1] = out[i][0]/scale, out[i][1]/scale
	}
	return out, [2]float64{qxy[0] / scale, qxy[1] / scale}
}

// direction is the rows' main direction by power iteration, kept at right angles to not, when
// given. Its starting vector is fixed, so the same memories draw the same map.
func direction(rows [][]float64, not []float64) []float64 {
	dims := len(rows[0])
	v := make([]float64, dims)
	for d := range v {
		v[d] = 1 / math.Sqrt(float64(d+1))
	}
	for range 50 {
		next := make([]float64, dims)
		for _, r := range rows {
			k := dot(r, v)
			for d := range next {
				next[d] += k * r[d]
			}
		}
		if not != nil {
			k := dot(next, not)
			for d := range next {
				next[d] -= k * not[d]
			}
		}
		n := math.Sqrt(dot(next, next))
		if n == 0 {
			return v
		}
		for d := range next {
			next[d] /= n
		}
		v = next
	}
	return v
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func cosine(a, b []float32) float64 {
	var ab, aa, bb float64
	for i := range a {
		ab += float64(a[i]) * float64(b[i])
		aa += float64(a[i]) * float64(a[i])
		bb += float64(b[i]) * float64(b[i])
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}
