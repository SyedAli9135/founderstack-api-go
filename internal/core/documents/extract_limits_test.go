package documents

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func docx(t *testing.T, paragraphs int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<w:document xmlns:w="x"><w:body>`))
	for i := 0; i < paragraphs; i++ {
		_, _ = w.Write([]byte(`<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`))
	}
	_, _ = w.Write([]byte(`</w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractText_RefusesADocxThatExpandsPastTheCap(t *testing.T) {
	// ~45 bytes per paragraph, and highly repetitive, so it deflates to almost nothing.
	bomb := docx(t, 50_000)
	if len(bomb) > 20_000 {
		t.Fatalf("test file isn't compressible enough to model a bomb: %d bytes", len(bomb))
	}

	orig := maxDocXMLBytes
	maxDocXMLBytes = 100_000
	t.Cleanup(func() { maxDocXMLBytes = orig })
	if _, err := ExtractText("big.docx", bomb); !errors.Is(err, ErrTooMuchText) {
		t.Fatalf("err = %v, want ErrTooMuchText", err)
	}

	maxDocXMLBytes = orig
	if text, err := ExtractText("ok.docx", docx(t, 20)); err != nil || !strings.Contains(text, "hello world") {
		t.Fatalf("a normal docx = (%q, %v), want its text", text, err)
	}
}

func TestExtractText_RefusesTextPastTheCap(t *testing.T) {
	orig := maxExtractedTextLen
	maxExtractedTextLen = 1000
	t.Cleanup(func() { maxExtractedTextLen = orig })

	if _, err := ExtractText("big.txt", []byte(strings.Repeat("a", 1001))); !errors.Is(err, ErrTooMuchText) {
		t.Fatalf("err = %v, want ErrTooMuchText", err)
	}
	if _, err := ExtractText("ok.md", []byte(strings.Repeat("a", 1000))); err != nil {
		t.Fatalf("text at the cap = %v, want it accepted", err)
	}
}
