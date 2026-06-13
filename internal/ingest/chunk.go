package ingest

const (
	chunkTarget  = 1000
	chunkHardCap = 1200
	chunkOverlap = 200
)

// ChunkConfig carries the three tunable chunking parameters. It exists so callers
// (notably the eval sweep) can drive the splitter across a grid of settings
// without touching the production defaults baked into ChunkText.
type ChunkConfig struct {
	Target  int // preferred chunk size in chars; split points center near this
	HardCap int // maximum chunk size in chars; never exceeded
	Overlap int // chars of overlap carried from one chunk into the next
}

// DefaultChunkConfig returns the production chunking configuration ChunkText
// uses. Expressed as a function rather than a package var to satisfy
// gochecknoglobals — a ChunkConfig cannot be a const.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{Target: chunkTarget, HardCap: chunkHardCap, Overlap: chunkOverlap}
}

// withDefaults returns a copy of c with any non-positive field replaced by the
// corresponding DefaultChunkConfig value, so a zero-valued or partially filled
// ChunkConfig degrades to production behavior rather than to a degenerate split.
func (c ChunkConfig) withDefaults() ChunkConfig {
	d := DefaultChunkConfig()
	if c.Target <= 0 {
		c.Target = d.Target
	}
	if c.HardCap <= 0 {
		c.HardCap = d.HardCap
	}
	if c.Overlap <= 0 {
		c.Overlap = d.Overlap
	}
	return c
}

// ChunkText splits text into overlapping segments targeting ~1000 chars with ~200-char overlap.
// Hard cap per chunk is 1200 chars. Splits prefer paragraph (\n\n) then newline then word
// boundary — never mid-word. Returns nil for empty or whitespace-only input.
func ChunkText(text string) []string {
	return chunkWithConfig(text, chunkTarget, chunkHardCap, chunkOverlap)
}

// ChunkTextWith splits text using an explicit ChunkConfig. Any non-positive field
// in cfg falls back to the DefaultChunkConfig value. All ChunkText invariants
// (hard cap respected, no mid-word splits, full coverage, no empty chunks) hold
// for any positive configuration. This is the entry point the eval sweep uses to
// vary chunk geometry; production paths keep calling ChunkText.
func ChunkTextWith(text string, cfg ChunkConfig) []string {
	c := cfg.withDefaults()
	return chunkWithConfig(text, c.Target, c.HardCap, c.Overlap)
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
	// prevSplit is the end (exclusive) of the previously emitted chunk. Each new
	// split point must land strictly after it: the overlap window backs `start`
	// up before prevSplit, so without this floor the same early boundary (e.g. a
	// section header's lone \n\n preceding a long single-newline list) gets
	// re-selected every iteration, emitting a staircase of ever-shrinking
	// degenerate tail chunks. Monotonic split points guarantee forward progress.
	prevSplit := 0

	for start < len(runes) {
		// Remaining runes all fit in one chunk
		if len(runes)-start <= hardCap {
			chunk := string(runes[start:])
			if isNonEmpty(chunk) {
				chunks = append(chunks, chunk)
			}
			break
		}

		splitAt := findSplitPoint(runes, start, target, hardCap, prevSplit)
		prevSplit = splitAt

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
// Among equal-quality boundaries, the one closest to start+target is preferred so
// chunks center near the target size rather than drifting toward the hard cap.
//
// minSplit is a floor: the returned split point is always > minSplit. The caller
// passes the previous chunk's split point so that an early boundary already
// consumed cannot be re-selected when the overlap window slides back over it —
// the cause of the degenerate-tail staircase. The hard-cut fallback (end) always
// satisfies the floor because end = start+hardCap exceeds any prior split that the
// overlap window could have backed `start` up behind.
func findSplitPoint(runes []rune, start, target, hardCap, minSplit int) int {
	end := start + hardCap
	if end > len(runes) {
		end = len(runes)
	}
	preferred := start + target
	if preferred > end {
		preferred = end
	}
	// pref floors the high-quality first pass at the target size; the forward
	// scan within [pref, end) then selects the boundary closest to the target
	// from above, rather than the one nearest the hard cap.
	// lo floors the widened fallback by two independent concerns: minSplit (the
	// staircase guard — never re-select a boundary at or before the prior split)
	// and minFragment (don't split off a leading chunk smaller than half a target,
	// so a lone "## HEADING\n\n" before a long list is absorbed forward, not
	// orphaned).
	minFragment := target / 2
	lo := max(start+minFragment, minSplit)
	pref := max(preferred, minSplit)

	// For each boundary type, try the preferred window first, then widen to the
	// full [lo, end) window. Quality ordering is preserved across the two windows.
	// The preferred window scans forward (first* — the earliest boundary >= target,
	// closest to target from above); the widened fallback scans backward (last* —
	// the latest boundary < target, closest to target from below). Either way the
	// chosen boundary is the best-quality one nearest the target.
	if pos, ok := firstParaBreak(runes, pref, end); ok {
		return pos
	}
	if pos, ok := lastParaBreak(runes, lo, end); ok {
		return pos
	}
	if pos, ok := firstNewline(runes, pref, end); ok {
		return pos
	}
	if pos, ok := lastNewline(runes, lo, end); ok {
		return pos
	}
	if pos, ok := firstSpace(runes, pref, end); ok {
		return pos
	}
	if pos, ok := lastSpace(runes, lo, end); ok {
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

// firstParaBreak returns the position after the first \n\n in runes[lo:end].
func firstParaBreak(runes []rune, lo, end int) (int, bool) {
	for i := lo + 1; i < end; i++ {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			return i + 1, true
		}
	}
	return 0, false
}

// firstNewline returns the position after the first \n in runes[lo:end].
func firstNewline(runes []rune, lo, end int) (int, bool) {
	for i := lo + 1; i < end; i++ {
		if runes[i] == '\n' {
			return i + 1, true
		}
	}
	return 0, false
}

// firstSpace returns the position after the first space/tab in runes[lo:end].
func firstSpace(runes []rune, lo, end int) (int, bool) {
	for i := lo + 1; i < end; i++ {
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
