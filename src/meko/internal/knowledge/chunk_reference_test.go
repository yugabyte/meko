package knowledge

import "strings"

// The splitter as it was before it packed by position: strings.Split at every level, a string per
// piece (one per character with no separator), and merge joining them. It made the chunks Meko
// stores, so the tests hold the new splitter to it, chunk for chunk. It isn't used outside tests.

func refSplitText(text string, size, overlap int) []string {
	return refSplitWith(text, separators, size, overlap)
}

func refSplitWith(text string, seps []string, size, overlap int) []string {
	sep, rest := seps[len(seps)-1], []string(nil)
	for i, s := range seps {
		if s == "" || strings.Contains(text, s) {
			sep, rest = s, seps[i+1:]
			break
		}
	}
	var pieces []string
	if sep == "" {
		pieces = strings.Split(text, "")
	} else {
		pieces = strings.Split(text, sep)
	}
	var out, fit []string
	for _, p := range pieces {
		if runes(p) <= size {
			fit = append(fit, p)
			continue
		}
		out = append(out, refMerge(fit, sep, size, overlap)...)
		fit = nil
		if len(rest) == 0 {
			out = append(out, p)
		} else {
			out = append(out, refSplitWith(p, rest, size, overlap)...)
		}
	}
	return append(out, refMerge(fit, sep, size, overlap)...)
}

func refMerge(pieces []string, sep string, size, overlap int) []string {
	sepLen := runes(sep)
	var out, cur []string
	total := 0
	emit := func() {
		if c := strings.TrimSpace(strings.Join(cur, sep)); c != "" {
			out = append(out, c)
		}
	}
	joined := func(n int) int { // what adding a piece of n characters costs
		if len(cur) > 0 {
			return n + sepLen
		}
		return n
	}
	for _, p := range pieces {
		n := runes(p)
		if total+joined(n) > size && len(cur) > 0 {
			emit()
			for total > 0 && (total > overlap || total+joined(n) > size) {
				total -= runes(cur[0])
				if len(cur) > 1 {
					total -= sepLen
				}
				cur = cur[1:]
			}
		}
		total += joined(n)
		cur = append(cur, p)
	}
	emit()
	return out
}

func refPackLines(text string) []string {
	var out, fit []string
	flush := func() {
		out = append(out, refMerge(fit, "\n", ChunkSize, ChunkOverlap)...)
		fit = nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if runes(line) <= ChunkSize {
			fit = append(fit, line)
			continue
		}
		flush()
		out = append(out, refSplitText(line, ChunkSize, ChunkOverlap)...)
	}
	flush()
	return out
}
