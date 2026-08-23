package documents

import (
	"archive/zip"
	"bytes"
	"testing"
)

// buildTestDocx constructs a minimal, real .docx file in memory —
// a zip containing just word/document.xml with the paragraphs given,
// each as a single <w:t> run. Real Word documents split a paragraph's
// text across multiple runs (formatting changes, spell-check markers,
// etc.); this only needs to prove extractDOCX correctly walks
// <w:p>/<w:r>/<w:t> and joins paragraphs with a blank line, not
// reproduce every real-world docx quirk.
func buildTestDocx(t *testing.T, paragraphs []string) []byte {
	t.Helper()

	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>`)
		body.WriteString(p)
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	body.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDOCX_JoinsParagraphsWithBlankLine(t *testing.T) {
	data := buildTestDocx(t, []string{"First paragraph.", "Second paragraph."})

	got, err := extractDOCX(data)
	if err != nil {
		t.Fatalf("extractDOCX() error = %v", err)
	}
	want := "First paragraph.\n\nSecond paragraph."
	if got != want {
		t.Fatalf("extractDOCX() = %q, want %q", got, want)
	}
}

func TestExtractDOCX_MissingDocumentXML(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_, _ = zw.Create("word/styles.xml") // present, but not document.xml
	_ = zw.Close()

	if _, err := extractDOCX(buf.Bytes()); err == nil {
		t.Fatal("extractDOCX() with no word/document.xml: error = nil, want an error")
	}
}

func TestExtractText_DispatchesByExtension(t *testing.T) {
	txt, err := ExtractText("notes.txt", []byte("hello"))
	if err != nil || txt != "hello" {
		t.Errorf("ExtractText(.txt) = (%q, %v), want (hello, nil)", txt, err)
	}

	md, err := ExtractText("README.MD", []byte("# heading")) // case-insensitive extension
	if err != nil || md != "# heading" {
		t.Errorf("ExtractText(.MD) = (%q, %v), want (# heading, nil)", md, err)
	}

	if _, err := ExtractText("archive.zip", []byte("junk")); err != ErrUnsupportedFileType {
		t.Errorf("ExtractText(.zip) error = %v, want ErrUnsupportedFileType", err)
	}
}
