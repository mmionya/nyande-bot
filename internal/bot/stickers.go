package bot

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
	_ "golang.org/x/image/webp"
)

//go:embed render_tgs.py
var renderTGSScript string

func (b *Bot) collectSticker(ctx context.Context, sticker *telegram.Sticker) ([]cachedAttachment, error) {
	attachment := cachedAttachment{Kind: "sticker", Name: "sticker.webp", MIME: "image/webp"}
	if sticker.IsVideo {
		attachment.Name, attachment.MIME = "sticker.webm", "video/webm"
	}
	if sticker.IsAnimated {
		attachment.Name, attachment.MIME = "sticker.tgs", "application/x-tgsticker"
	}
	if sticker.Thumbnail != nil {
		attachment.PreviewFileID = sticker.Thumbnail.FileID
	}
	if !b.cfg.LLMVision {
		return []cachedAttachment{attachment}, nil
	}
	limit := int64(20 * 1024 * 1024)
	if b.cfg.MaxFileSize > 0 && b.cfg.MaxFileSize < limit {
		limit = b.cfg.MaxFileSize
	}
	var err error
	if sticker.FileSize > limit {
		err = errors.New(resources.Get("telegram.media.file_too_large"))
	} else {
		attachment.Data, _, err = b.telegram.DownloadFile(ctx, sticker.FileID, limit)
	}
	if err != nil {
		if attachment.PreviewFileID == "" {
			return nil, err
		}
		if limit > 1024*1024 {
			limit = 1024 * 1024
		}
		attachment.Data, _, err = b.telegram.DownloadFile(ctx, attachment.PreviewFileID, limit)
		if err != nil {
			return nil, err
		}
		attachment.Kind, attachment.Name, attachment.MIME = "sticker_preview", "preview.webp", "image/webp"
	}
	return []cachedAttachment{attachment}, nil
}

func (b *Bot) readSticker(ctx context.Context, attachment cachedAttachment) ([]llm.Image, string, error) {
	count := b.cfg.LLMVideoFrames
	if count <= 0 {
		count = 3
	}
	count = min(count, 6)
	renderCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	var images []llm.Image
	var err error
	switch filepath.Ext(attachment.Name) {
	case ".tgs":
		images, err = renderTGS(renderCtx, attachment.Data, count, b.cfg.StickerPythonPath)
	case ".webm":
		images, err = extractVideoFramesWithFormat(renderCtx, attachment.Data, attachment.Name, count, true)
	default:
		images, err = stickerImage(attachment.Data)
	}
	cancel()
	contextText := resources.Get("llm.prompt.sticker_context")
	if err != nil && attachment.PreviewFileID != "" && attachment.Kind != "sticker_preview" && ctx.Err() == nil {
		previewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		limit := int64(1024 * 1024)
		if b.cfg.MaxFileSize > 0 && b.cfg.MaxFileSize < limit {
			limit = b.cfg.MaxFileSize
		}
		if data, _, downloadErr := b.telegram.DownloadFile(previewCtx, attachment.PreviewFileID, limit); downloadErr == nil {
			images, err = stickerImage(data)
			attachment.Kind = "sticker_preview"
		}
	}
	if attachment.Kind == "sticker_preview" {
		contextText += "\n" + resources.Get("llm.prompt.sticker_preview")
	}
	return images, contextText, err
}

// Convert WebP thumbnails/static stickers into a reliably supported vision format.
func stickerImage(data []byte) ([]llm.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4*1024*1024/cfg.Height {
		return nil, errors.New(resources.Get("telegram.sticker.failed"))
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := png.Encode(&output, im); err != nil {
		return nil, err
	}
	return []llm.Image{{MIME: "image/png", Data: output.Bytes()}}, nil
}

func renderTGS(ctx context.Context, data []byte, count int, pythonPath string) ([]llm.Image, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	const maxJSON = 4 * 1024 * 1024
	animation, err := io.ReadAll(io.LimitReader(r, maxJSON+1))
	if err != nil {
		return nil, err
	}
	var metadata struct {
		Width  int     `json:"w"`
		Height int     `json:"h"`
		Start  float64 `json:"ip"`
		End    float64 `json:"op"`
		Assets []struct {
			Path      string `json:"p"`
			Directory string `json:"u"`
		} `json:"assets"`
	}
	if len(animation) > maxJSON || json.Unmarshal(animation, &metadata) != nil || metadata.Width < 1 || metadata.Height < 1 ||
		metadata.Width > 1024 || metadata.Height > 1024 || metadata.End <= metadata.Start || metadata.End-metadata.Start > 600 {
		return nil, errors.New(resources.Get("telegram.sticker.failed"))
	}
	// TGS stickers are vector animations; never let the renderer load external files.
	for _, asset := range metadata.Assets {
		if asset.Path != "" || asset.Directory != "" {
			return nil, errors.New(resources.Get("telegram.sticker.failed"))
		}
	}
	dir, err := os.MkdirTemp("", "nyande-sticker-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "animation.json")
	if err := os.WriteFile(input, animation, 0o600); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, firstString(pythonPath, "python3"), "-c", renderTGSScript, input, dir, strconv.Itoa(count))
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("render TGS: %w", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "frame-*.png"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 || len(paths) > count {
		return nil, errors.New(resources.Get("telegram.sticker.failed"))
	}
	var images []llm.Image
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		images = append(images, llm.Image{MIME: "image/png", Data: data})
	}
	return images, nil
}
