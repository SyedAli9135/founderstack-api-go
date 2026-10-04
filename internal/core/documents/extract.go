package documents

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Upload size is capped at the API, but a compressed file can expand far
// beyond it (a DOCX's XML deflates ~1000:1), so extraction bounds its own
// output. Vars so tests can shrink them.
var (
	maxDocXMLBytes      int64 = 100 << 20
	maxExtractedTextLen int64 = 20 << 20
)

var ErrTooMuchText = fmt.Errorf("documents: file contains too much text to index")

// ErrUnsupportedFileType: checked earlier in internal/api/documents too,
// but kept here so ExtractText never silently returns empty text for a
// file type it doesn't understand.
var ErrUnsupportedFileType = fmt.Errorf("documents: unsupported file type")

// ExtractText pulls plain text out of data, dispatching on filename's
// extension. PDF and DOCX need real parsing; TXT/MD are read as-is.
func ExtractText(filename string, data []byte) (string, error) {
	var text string
	var err error
	switch {
	case hasSuffixFold(filename, ".pdf"):
		text, err = extractPDF(data)
	case hasSuffixFold(filename, ".docx"):
		text, err = extractDOCX(data)
	case hasSuffixFold(filename, ".txt"), hasSuffixFold(filename, ".md"):
		text = string(data)
	default:
		return "", ErrUnsupportedFileType
	}
	if err != nil {
		return "", err
	}
	if int64(len(text)) > maxExtractedTextLen {
		return "", ErrTooMuchText
	}
	return text, nil
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

func extractPDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("documents: open pdf: %w", err)
	}
	textReader, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("documents: extract pdf text: %w", err)
	}
	text, err := io.ReadAll(io.LimitReader(textReader, maxExtractedTextLen+1))
	if err != nil {
		return "", fmt.Errorf("documents: read pdf text: %w", err)
	}
	return string(text), nil
}

// extractDOCX reads word/document.xml out of the .docx zip and pulls text
// out of every <w:t> run, joining paragraphs (<w:p> boundaries) with a
// blank line. Hand-rolled on archive/zip + encoding/xml rather than a
// docx-parsing dependency — no tables/headers/footers/tracked changes, so
// a real dependency wasn't justified.
func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("documents: open docx zip: %w", err)
	}

	var docXML *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML = f
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("documents: docx has no word/document.xml")
	}

	if int64(docXML.UncompressedSize64) > maxDocXMLBytes {
		return "", ErrTooMuchText
	}
	rc, err := docXML.Open()
	if err != nil {
		return "", fmt.Errorf("documents: open word/document.xml: %w", err)
	}
	defer rc.Close()

	// The declared size is attacker-controlled, so the read is bounded too.
	limited := &io.LimitedReader{R: rc, N: maxDocXMLBytes + 1}
	var out, para strings.Builder
	dec := xml.NewDecoder(limited)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if limited.N <= 0 || int64(out.Len()) > maxExtractedTextLen {
			return "", ErrTooMuchText
		}
		if err != nil {
			return "", fmt.Errorf("documents: parse docx xml: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// Match on local name only, ignoring the namespace prefix —
			// DOCX producers don't all use the same prefix.
			if t.Name.Local == "t" {
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", fmt.Errorf("documents: decode docx text run: %w", err)
				}
				para.WriteString(text)
			}
		case xml.EndElement:
			if t.Name.Local == "p" && para.Len() > 0 {
				out.WriteString(para.String())
				out.WriteString("\n\n")
				para.Reset()
			}
		}
	}

	return strings.TrimSpace(out.String()), nil
}
