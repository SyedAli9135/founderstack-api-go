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

// ErrUnsupportedFileType means the upload's extension/mime type isn't
// one of the 4 workflow 6 supports (PDF, DOCX, TXT, MD) — checked before
// this package is ever reached (see internal/api/documents), but kept as
// a real error here too so ExtractText never silently returns empty text
// for a file type it doesn't understand.
var ErrUnsupportedFileType = fmt.Errorf("documents: unsupported file type")

// ExtractText pulls plain text out of data, dispatching on filename's
// extension. PDF and DOCX need real parsing; TXT/MD are read as-is.
func ExtractText(filename string, data []byte) (string, error) {
	switch {
	case hasSuffixFold(filename, ".pdf"):
		return extractPDF(data)
	case hasSuffixFold(filename, ".docx"):
		return extractDOCX(data)
	case hasSuffixFold(filename, ".txt"), hasSuffixFold(filename, ".md"):
		return string(data), nil
	default:
		return "", ErrUnsupportedFileType
	}
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
	text, err := io.ReadAll(textReader)
	if err != nil {
		return "", fmt.Errorf("documents: read pdf text: %w", err)
	}
	return string(text), nil
}

// extractDOCX reads word/document.xml out of the .docx zip and pulls
// text out of every <w:t> run, joining paragraphs (<w:p> boundaries)
// with a blank line. Hand-rolled on stdlib archive/zip + encoding/xml
// rather than a docx-parsing dependency — the actual text-extraction
// need here is narrow enough (no tables, headers/footers, tracked
// changes, or styling) that a real dependency wasn't justified, same
// "don't add one for less than it earns" reasoning as everywhere else
// BYOK/integrations/documents touches a format in this codebase.
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

	rc, err := docXML.Open()
	if err != nil {
		return "", fmt.Errorf("documents: open word/document.xml: %w", err)
	}
	defer rc.Close()

	var out, para strings.Builder
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("documents: parse docx xml: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// Word namespaces this element (commonly "w:t"); matching on
			// the local name only (ignoring the namespace prefix) is
			// deliberate — DOCX producers don't all use the same prefix,
			// but the local name is stable.
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
