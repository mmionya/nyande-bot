// Package documents extracts bounded, plain-text context from document attachments.
package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"os/exec"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/resources"
)

const (
	MaxFileSize     = 20 * 1024 * 1024
	MaxTextRunes    = 40000
	MaxDocuments    = 4
	MaxRequestRunes = 80000
	maxExpandedSize = 64 * 1024 * 1024
)

// Document contains extracted text, never the original file bytes.
type Document struct {
	Name       string   `json:"name"`
	Text       string   `json:"text"`
	Truncated  bool     `json:"truncated,omitempty"`
	Warning    string   `json:"warning,omitempty"`
	ImageNames []string `json:"image_names,omitempty"`
	Images     []Image  `json:"-"`
}

type Image struct {
	MIME string
	Data []byte
}

var (
	ErrTooLarge  = errors.New(resources.Get("documents.error.too_large"))
	ErrNoText    = errors.New(resources.Get("documents.error.no_text"))
	errTextLimit = errors.New("document text limit reached")
)

func format(name, mimeType string) string {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".pdf", ".docx", ".pptx", ".xlsx":
		return ext[1:]
	case ".odt", ".ods", ".odp", ".odf":
		return "odf"
	}
	mimeType, _, _ = mime.ParseMediaType(strings.ToLower(mimeType))
	switch mimeType {
	case "application/pdf":
		return "pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.oasis.opendocument.text", "application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.presentation", "application/vnd.oasis.opendocument.formula":
		return "odf"
	}
	return ""
}

func Supported(name, mimeType string) bool { return format(name, mimeType) != "" }

// Extract does not execute macros, follow external relationships, or perform OCR.
func Extract(ctx context.Context, name, mimeType string, data []byte) (Document, error) {
	result := Document{Name: name}
	if len(data) > MaxFileSize {
		return result, ErrTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	out := &textBuffer{}
	var err error
	kind := format(name, mimeType)
	if kind == "pdf" {
		err = extractPDF(ctx, data, out)
	} else if kind != "" {
		var archive *documentArchive
		archive, err = openArchive(ctx, data)
		if err == nil {
			switch kind {
			case "docx":
				err = archive.richText("word/document.xml", out, false)
			case "pptx":
				err = archive.presentation(out)
			case "xlsx":
				err = archive.spreadsheet(out)
			case "odf":
				err = archive.richText("content.xml", out, true)
			}
		}
	} else {
		return result, errors.New(resources.Get("documents.error.unsupported"))
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil && !errors.Is(err, errTextLimit) {
		return result, err
	}
	result.Text = strings.TrimSpace(out.String())
	result.Truncated = out.truncated || errors.Is(err, errTextLimit)
	if result.Text == "" {
		return result, ErrNoText
	}
	return result, nil
}

type textBuffer struct {
	strings.Builder
	runes     int
	truncated bool
}

func (b *textBuffer) add(text string) error {
	remaining := MaxTextRunes - b.runes
	for i := range text {
		if remaining == 0 {
			b.WriteString(text[:i])
			b.truncated = true
			return errTextLimit
		}
		remaining--
		b.runes++
	}
	b.WriteString(text)
	return nil
}

// Drain subprocess output while retaining at most a bounded UTF-8 prefix.
type limitedOutput struct {
	data      []byte
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := min(len(p), MaxTextRunes*utf8.UTFMax-len(b.data))
	b.data = append(b.data, p[:n]...)
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

func extractPDF(ctx context.Context, data []byte, out *textBuffer) error {
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	output := &limitedOutput{}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New(resources.Get("documents.error.pdf_dependency"))
		}
		return errors.New(resources.Get("documents.error.invalid_pdf"))
	}
	out.truncated = output.truncated
	return out.add(strings.ReplaceAll(strings.ToValidUTF8(string(output.data), ""), "\f", "\n"))
}

type documentArchive struct {
	ctx   context.Context
	files map[string]*zip.File
}

func openArchive(ctx context.Context, data []byte) (*documentArchive, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New(resources.Get("documents.error.invalid_office"))
	}
	if len(r.File) > 4096 {
		return nil, ErrTooLarge
	}
	a := &documentArchive{ctx: ctx, files: make(map[string]*zip.File, len(r.File))}
	var total uint64
	for _, f := range r.File {
		if f.UncompressedSize64 > maxExpandedSize-total {
			return nil, ErrTooLarge
		}
		total += f.UncompressedSize64
		if _, exists := a.files[f.Name]; exists {
			return nil, errors.New(resources.Get("documents.error.invalid_office"))
		}
		a.files[f.Name] = f
	}
	return a, nil
}

func (a *documentArchive) read(name string) ([]byte, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	f := a.files[name]
	if f == nil {
		return nil, errors.New(resources.Get("documents.error.invalid_office"))
	}
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxExpandedSize+1))
	if len(data) > maxExpandedSize {
		return nil, ErrTooLarge
	}
	return data, err
}
