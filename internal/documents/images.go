package documents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Read supplies text and, when enabled, embedded pictures (or rendered PDF pages)
// to the same model. It never loads remote image relationships.
func Read(ctx context.Context, name, mimeType string, data []byte, maxImages int) (Document, error) {
	doc, textErr := Extract(ctx, name, mimeType, data)
	if len(data) > MaxFileSize || (textErr != nil && !errors.Is(textErr, ErrNoText)) {
		return doc, textErr
	}
	if maxImages <= 0 {
		if textErr == nil {
			doc.Warning = resources.Get("documents.warning.images_disabled")
		}
		return doc, textErr
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var imageErr error
	if format(name, mimeType) == "pdf" {
		var total int
		doc.Images, total, imageErr = renderPDF(ctx, data, min(maxImages, 6))
		if imageErr == nil {
			doc.Warning = resources.Format("documents.warning.pages", map[string]any{"rendered": len(doc.Images), "total": total})
			for i := range doc.Images {
				doc.ImageNames = append(doc.ImageNames, fmt.Sprintf("page %d", i+1))
			}
		}
	} else {
		doc.Images, doc.ImageNames, doc.Warning, imageErr = embeddedImages(ctx, name, mimeType, data, min(maxImages, 6))
	}
	if ctx.Err() != nil {
		return doc, ctx.Err()
	}
	if imageErr != nil {
		if textErr != nil {
			return doc, imageErr
		}
		doc.Warning = resources.Get("documents.warning.images_failed") + " " + imageErr.Error()
	}
	if textErr != nil && len(doc.Images) == 0 {
		return doc, textErr
	}
	return doc, nil
}

func embeddedImages(ctx context.Context, name, mimeType string, data []byte, limit int) ([]Image, []string, string, error) {
	a, err := openArchive(ctx, data)
	if err != nil {
		return nil, nil, "", err
	}
	prefix := map[string]string{"docx": "word/media/", "pptx": "ppt/media/", "xlsx": "xl/media/", "odf": "Pictures/"}[format(name, mimeType)]
	var candidates []string
	for name := range a.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".svg", ".emf", ".wmf":
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	var images []Image
	var names []string
	for _, name := range candidates {
		if len(images) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		data, err := a.read(name)
		if err != nil {
			return nil, nil, "", err
		}
		picture, err := normalizeImage(data)
		if err != nil {
			continue
		}
		images = append(images, picture)
		names = append(names, name)
	}
	warning := ""
	if len(images) < len(candidates) {
		warning = resources.Format("documents.warning.embedded_images", map[string]any{"included": len(images), "total": len(candidates)})
	}
	return images, names, warning, nil
}

func normalizeImage(data []byte) (Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 40000000/cfg.Height {
		return Image{}, ErrTooLarge
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, err
	}
	if max(cfg.Width, cfg.Height) > 1600 {
		width, height := max(1, cfg.Width*1600/max(cfg.Width, cfg.Height)), max(1, cfg.Height*1600/max(cfg.Width, cfg.Height))
		scaled := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Src, nil)
		source = scaled
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		return Image{}, err
	}
	return Image{MIME: "image/png", Data: output.Bytes()}, nil
}
