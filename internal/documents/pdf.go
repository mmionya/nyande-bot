package documents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

func renderPDF(ctx context.Context, data []byte, limit int) ([]Image, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "nyande-pdf-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, 0, err
	}
	info := exec.CommandContext(ctx, "pdfinfo", input)
	info.Env = append(os.Environ(), "LC_ALL=C")
	infoOutput := &limitedOutput{}
	info.Stdout = infoOutput
	if err := info.Run(); err != nil {
		return nil, 0, pdfRenderError(ctx, err)
	}
	pages := 0
	for _, line := range strings.Split(string(infoOutput.data), "\n") {
		if value, ok := strings.CutPrefix(line, "Pages:"); ok {
			pages, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if pages <= 0 {
		return nil, 0, errors.New(resources.Get("documents.error.invalid_pdf"))
	}
	var images []Image
	for page := 1; page <= min(pages, limit); page++ {
		// Render one page at a time, with fixed output dimensions and filename.
		prefix := filepath.Join(dir, "page")
		cmd := exec.CommandContext(ctx, "pdftoppm", "-f", strconv.Itoa(page), "-l", strconv.Itoa(page),
			"-singlefile", "-scale-to", "1600", "-jpeg", input, prefix)
		if err := cmd.Run(); err != nil {
			return nil, 0, pdfRenderError(ctx, err)
		}
		image, err := os.ReadFile(prefix + ".jpg")
		if err != nil {
			return nil, 0, err
		}
		images = append(images, Image{MIME: "image/jpeg", Data: image})
	}
	return images, pages, nil
}

func pdfRenderError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New(resources.Get("documents.error.pdf_dependency"))
	}
	return errors.New(resources.Get("documents.error.invalid_pdf"))
}
