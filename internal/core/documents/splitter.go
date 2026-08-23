package documents

import "strings"

// chunkSeparators is the cascade SplitText tries in order — paragraph
// breaks first, then line breaks, then sentence-ish breaks, then plain
// whitespace. Mirrors LangChain's RecursiveCharacterTextSplitter's
// separator-cascade idea (there's no ready-made Go package for it, per
// WORKFLOW_PLAN_GO.md workflow 6): try to keep whole paragraphs/sentences
// together, only falling back to a cruder split when a piece is still
// too big.
var chunkSeparators = []string{"\n\n", "\n", ". ", " "}

const (
	// DefaultChunkSize and DefaultChunkOverlap match workflow 6's spec:
	// ~1024 characters per chunk, 128 characters of overlap between
	// consecutive chunks so a fact split across a chunk boundary is
	// still findable from either side.
	DefaultChunkSize    = 1024
	DefaultChunkOverlap = 128
)

// SplitText breaks text into chunks of at most chunkSize characters
// each, carrying chunkOverlap characters from the end of one chunk into
// the start of the next. Returns nil for empty/whitespace-only input.
func SplitText(text string, chunkSize, chunkOverlap int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	pieces := splitRecursive(text, chunkSeparators, chunkSize)
	return mergePieces(pieces, chunkSize, chunkOverlap)
}

// splitRecursive breaks text on the first separator in seps, then
// recurses into any resulting piece still longer than chunkSize using
// the remaining separators. A piece with no separator left to try (fell
// through the whole cascade — one very long word, e.g.) is returned
// as-is, oversized: silently truncating it would drop content, which is
// worse than one chunk running over the target size.
func splitRecursive(text string, seps []string, chunkSize int) []string {
	if len(text) <= chunkSize || len(seps) == 0 {
		return []string{text}
	}

	sep := seps[0]
	rest := seps[1:]
	parts := strings.Split(text, sep)

	var out []string
	for i, part := range parts {
		if part == "" {
			continue
		}
		// Put the separator back (except after the last part) so
		// mergePieces reproduces the original text's whitespace and
		// punctuation instead of silently collapsing it away.
		if i < len(parts)-1 {
			part += sep
		}
		if len(part) > chunkSize {
			out = append(out, splitRecursive(part, rest, chunkSize)...)
		} else {
			out = append(out, part)
		}
	}
	return out
}

// mergePieces greedily packs consecutive small pieces from splitRecursive
// into chunks up to chunkSize (so a chunk isn't just "one paragraph" when
// several short ones would fit together), carrying the last chunkOverlap
// characters of each chunk into the start of the next.
func mergePieces(pieces []string, chunkSize, chunkOverlap int) []string {
	var chunks []string
	var current strings.Builder

	flush := func() {
		if current.Len() == 0 {
			return
		}
		chunks = append(chunks, strings.TrimSpace(current.String()))
	}

	for _, p := range pieces {
		if current.Len() > 0 && current.Len()+len(p) > chunkSize {
			flush()
			overlap := lastNChars(current.String(), chunkOverlap)
			current.Reset()
			current.WriteString(overlap)
		}
		current.WriteString(p)
	}
	flush()

	return chunks
}

func lastNChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
