package ingest

const (
	chunkTarget  = 1000
	chunkHardCap = 1200
	chunkOverlap = 200
)

// ChunkText splits text into overlapping segments targeting ~1000 chars with ~200-char overlap.
// Hard cap per chunk is 1200 chars. Splits prefer paragraph (\n\n) then newline then word
// boundary — never mid-word. Returns nil for empty or whitespace-only input.
func ChunkText(text string) []string {
	return chunkWithConfig(text, chunkTarget, chunkHardCap, chunkOverlap)
}

func chunkWithConfig(text string, target, hardCap, overlap int) []string {
	runes := []rune(text)
	if !isNonEmpty(text) {
		return nil
	}
	if len(runes) <= hardCap {
		return []string{text}
	}

	var chunks []string
	start := 0

	for start < len(runes) {
		// Remaining runes all fit in one chunk
		if len(runes)-start <= hardCap {
			chunk := string(runes[start:])
			if isNonEmpty(chunk) {
				chunks = append(chunks, chunk)
			}
			break
		}

		splitAt := findSplitPoint(runes, start, target, hardCap)

		chunk := string(runes[start:splitAt])
		if isNonEmpty(chunk) {
			chunks = append(chunks, chunk)
		}

		// Back up overlap chars before the split and snap to a word boundary
		// so the next chunk begins at a clean word start.
		nextStart := splitAt - overlap
		if nextStart < start+1 {
			nextStart = start + 1
		}
		nextStart = wordBoundaryForward(runes, nextStart)
		if nextStart >= splitAt {
			nextStart = start + 1 // guard against infinite loop on pathological input
		}
		start = nextStart
	}

	return chunks
}

// findSplitPoint returns the index (exclusive end) of the best split point
// for the slice runes[start:]. Boundary quality ranks as \n\n > \n > space > hard cut.
// Among equal-quality boundaries, the one at or after start+target is preferred so
// chunks land near the target size rather than splitting as early as possible.
func findSplitPoint(runes []rune, start, target, hardCap int) int {
	end := start + hardCap
	if end > len(runes) {
		end = len(runes)
	}
	preferred := start + target
	if preferred > end {
		preferred = end
	}

	// For each boundary type, try the preferred window first, then widen to the
	// full [start, end) window. Quality ordering is preserved across the two windows.
	if pos, ok := lastParaBreak(runes, preferred, end); ok {
		return pos
	}
	if pos, ok := lastParaBreak(runes, start, end); ok {
		return pos
	}
	if pos, ok := lastNewline(runes, preferred, end); ok {
		return pos
	}
	if pos, ok := lastNewline(runes, start, end); ok {
		return pos
	}
	if pos, ok := lastSpace(runes, preferred, end); ok {
		return pos
	}
	if pos, ok := lastSpace(runes, start, end); ok {
		return pos
	}

	return end
}

// lastParaBreak returns the position after the last \n\n in runes[lo:end].
func lastParaBreak(runes []rune, lo, end int) (int, bool) {
	for i := end - 1; i >= lo+1; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			pos := i + 1
			if pos > end {
				return end, true
			}
			return pos, true
		}
	}
	return 0, false
}

// lastNewline returns the position after the last \n in runes[lo:end].
func lastNewline(runes []rune, lo, end int) (int, bool) {
	for i := end - 1; i >= lo+1; i-- {
		if runes[i] == '\n' {
			return i + 1, true
		}
	}
	return 0, false
}

// lastSpace returns the position after the last space/tab in runes[lo:end].
func lastSpace(runes []rune, lo, end int) (int, bool) {
	for i := end - 1; i >= lo+1; i-- {
		if runes[i] == ' ' || runes[i] == '\t' {
			return i + 1, true
		}
	}
	return 0, false
}

// wordBoundaryForward advances pos to the start of the next word when pos
// lands mid-word or on whitespace, ensuring overlap regions begin cleanly.
func wordBoundaryForward(runes []rune, pos int) int {
	if pos <= 0 || pos >= len(runes) {
		return pos
	}
	if !isSpace(runes[pos]) && !isSpace(runes[pos-1]) {
		for pos < len(runes) && !isSpace(runes[pos]) {
			pos++
		}
		for pos < len(runes) && isSpace(runes[pos]) {
			pos++
		}
	} else if isSpace(runes[pos]) {
		for pos < len(runes) && isSpace(runes[pos]) {
			pos++
		}
	}
	return pos
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func isNonEmpty(s string) bool {
	for _, r := range s {
		if !isSpace(r) {
			return true
		}
	}
	return false
}
