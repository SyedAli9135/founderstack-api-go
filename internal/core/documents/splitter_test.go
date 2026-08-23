package documents

import (
	"strings"
	"testing"
)

func TestSplitText_EmptyInput(t *testing.T) {
	if got := SplitText("", 1024, 128); got != nil {
		t.Errorf("SplitText(\"\") = %v, want nil", got)
	}
	if got := SplitText("   \n\t  ", 1024, 128); got != nil {
		t.Errorf("SplitText(whitespace) = %v, want nil", got)
	}
}

func TestSplitText_ShortTextIsOneChunk(t *testing.T) {
	text := "This is a short document."
	got := SplitText(text, DefaultChunkSize, DefaultChunkOverlap)
	if len(got) != 1 || got[0] != text {
		t.Fatalf("SplitText(short) = %+v, want one chunk matching the input", got)
	}
}

func TestSplitText_LongTextProducesMultipleChunks(t *testing.T) {
	// 10 paragraphs, each ~150 chars — well over one 1024-char chunk in
	// total, so this must produce more than one chunk.
	var paras []string
	for i := 0; i < 10; i++ {
		paras = append(paras, strings.Repeat("word ", 30))
	}
	text := strings.Join(paras, "\n\n")

	chunks := SplitText(text, 1024, 128)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks for %d-char input, want at least 2", len(chunks), len(text))
	}
	for i, c := range chunks {
		if len(c) > 1024+128 { // soft target + max reasonable overlap slack
			t.Errorf("chunk %d is %d chars, suspiciously over chunkSize+overlap", i, len(c))
		}
	}
}

func TestSplitText_ConsecutiveChunksOverlap(t *testing.T) {
	text := strings.Repeat("alpha beta gamma delta epsilon zeta ", 60) // ~2200 chars
	chunks := SplitText(text, 500, 100)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2 to test overlap", len(chunks))
	}

	for i := 0; i < len(chunks)-1; i++ {
		// The tail of chunk i and the head of chunk i+1 should share
		// some text — a full character-exact overlap isn't guaranteed
		// (mergePieces trims whitespace), so this checks for a shared
		// word rather than an exact substring match.
		tailWords := strings.Fields(chunks[i])
		headWords := strings.Fields(chunks[i+1])
		if len(tailWords) == 0 || len(headWords) == 0 {
			t.Fatalf("chunk %d or %d has no words", i, i+1)
		}
		lastWord := tailWords[len(tailWords)-1]
		found := false
		for _, w := range headWords[:min(5, len(headWords))] {
			if w == lastWord {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("chunk %d's last word %q not found near the start of chunk %d (%v) — overlap may be missing",
				i, lastWord, i+1, headWords[:min(5, len(headWords))])
		}
	}
}

func TestSplitText_OversizedUnsplittableTokenIsNotDropped(t *testing.T) {
	// One giant "word" with no separators at all — splitRecursive falls
	// through the whole cascade and must still return it, not truncate.
	huge := strings.Repeat("x", 5000)
	chunks := SplitText(huge, 1024, 128)
	if len(chunks) != 1 || chunks[0] != huge {
		t.Fatalf("got %d chunks (first len=%d), want the single 5000-char token preserved whole", len(chunks), len(chunks[0]))
	}
}
