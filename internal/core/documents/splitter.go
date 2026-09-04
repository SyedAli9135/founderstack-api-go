package documents

import "strings"

// chunkSeparators is the cascade SplitText tries in order — paragraph,
// line, sentence-ish, then plain whitespace — mirroring LangChain's
// RecursiveCharacterTextSplitter: keep whole paragraphs/sentences
// together, only falling back to a cruder split when a piece is too big.
var chunkSeparators = []string{"\n\n", "\n", ". ", " "}

const (
	// 128 chars of overlap so a fact split across a chunk boundary is
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
// recurses into any resulting piece still longer than chunkSize using the
// remaining separators. A piece that falls through the whole cascade (one
// very long word) is returned as-is, oversized — truncating would drop
// content, worse than a chunk running over target size.
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
		// mergePieces reproduces the original whitespace/punctuation.
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

// mergePieces greedily packs consecutive small pieces into chunks up to
// chunkSize (so a chunk isn't just "one paragraph" when several short ones
// would fit), carrying the last chunkOverlap chars into the next chunk.
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
