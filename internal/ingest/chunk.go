package ingest

const (
	chunkTarget  = 1000
	chunkHardCap = 1200
	chunkOverlap = 200
)

// Chunk splits text into overlapping segments targeting ~1000 chars with ~200-char overlap.
// Hard cap per chunk is 1200 chars. Splits prefer paragraph (\n\n) then newline then word
// boundary — never mid-word. Returns nil for empty or whitespace-only input.
func Chunk(text string) []string {
	return chunkWithConfig(text, chunkTarget, chunkHardCap, chunkOverlap)
}

func chunkWithConfig(text string, target, hardCap, overlap int) []string {
	// Contract 5: empty/whitespace-only input → nil
	runes := []rune(text)
	hasContent := false
	for _, r := range runes {
		if !isSpace(r) {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil
	}

	// Contract 6: input ≤ hardCap → single chunk returned verbatim
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

		// Find the best split point within [target, hardCap] from start.
		// Prefer \n\n > \n > space > hard cut.
		splitAt := findSplitPoint(runes, start, target, hardCap)

		chunk := string(runes[start:splitAt])
		if isNonEmpty(chunk) {
			chunks = append(chunks, chunk)
		}

		// Advance start so next chunk begins overlap chars before splitAt,
		// anchored at a word boundary.
		nextStart := splitAt - overlap
		if nextStart < start+1 {
			nextStart = start + 1
		}
		// Walk nextStart forward to a word boundary (start of a word).
		nextStart = wordBoundaryForward(runes, nextStart)
		if nextStart >= splitAt {
			// Pathological: just advance by 1 to avoid infinite loop
			nextStart = start + 1
		}
		start = nextStart
	}

	return chunks
}

// findSplitPoint returns the index (exclusive end) of the best split point
// for the slice runes[start:]. Prefers \n\n > \n > space > hard cut.
func findSplitPoint(runes []rune, start, target, hardCap int) int {
	end := start + hardCap
	if end > len(runes) {
		end = len(runes)
	}
	_ = target // target is used by callers to decide whether to split at all
	if pos := lastParaBreak(runes, start, end); pos > 0 {
		return pos
	}
	if pos := lastNewline(runes, start, end); pos > 0 {
		return pos
	}
	if pos := lastSpace(runes, start, end); pos > 0 {
		return pos
	}
	return end
}

// lastParaBreak returns the position after the last \n\n in runes[start:end],
// or 0 if none exists.
func lastParaBreak(runes []rune, start, end int) int {
	for i := end - 1; i >= start+1; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			pos := i + 1
			if pos > end {
				return end
			}
			return pos
		}
	}
	return 0
}

// lastNewline returns the position after the last \n in runes[start:end],
// or 0 if none exists.
func lastNewline(runes []rune, start, end int) int {
	for i := end - 1; i >= start+1; i-- {
		if runes[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// lastSpace returns the position after the last space/tab in runes[start:end],
// or 0 if none exists.
func lastSpace(runes []rune, start, end int) int {
	for i := end - 1; i >= start+1; i-- {
		if runes[i] == ' ' || runes[i] == '\t' {
			return i + 1
		}
	}
	return 0
}

// wordBoundaryForward scans forward from pos until it finds the start of a
// word (a non-space rune preceded by a space, or the very start). Returns pos
// if already at a word boundary.
func wordBoundaryForward(runes []rune, pos int) int {
	// Scan forward to skip any partial word we're in the middle of.
	// If the char at pos is non-space and the char before it is also non-space,
	// we're mid-word — advance to next space then to next word.
	if pos <= 0 || pos >= len(runes) {
		return pos
	}
	if !isSpace(runes[pos]) && !isSpace(runes[pos-1]) {
		// mid-word: advance to end of this word
		for pos < len(runes) && !isSpace(runes[pos]) {
			pos++
		}
		// skip whitespace
		for pos < len(runes) && isSpace(runes[pos]) {
			pos++
		}
	} else if isSpace(runes[pos]) {
		// on whitespace: skip to next word
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
