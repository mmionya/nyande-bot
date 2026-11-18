package bot

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

// Only automatic downloads get this presentation rule. Explicit /video uploads
// bypass it, and the media cache keeps the original file for later conversions.
func (b *Bot) downloadedUpload(ctx context.Context, item downloader.Media) (telegram.Upload, error) {
	upload := telegram.Upload{Kind: item.Kind, Name: item.Name, MIME: item.MIME, Data: item.Data, URL: item.URL}
	if err := ctx.Err(); err != nil {
		return upload, err
	}
	if (item.Kind != "video" && item.Kind != "animation") || len(item.Data) == 0 {
		return upload, nil
	}
	if item.Kind == "animation" && (item.MIME == "image/gif" || strings.EqualFold(filepath.Ext(item.Name), ".gif")) {
		return upload, nil
	}
	info, err := b.probeVideo(ctx, item.Data, item.Name)
	if err != nil {
		if ctx.Err() != nil {
			return upload, ctx.Err()
		}
		// An unreadable or missing probe is not evidence that audio is absent.
		log.Printf(resources.Get("telegram.log.animation_prepare_failed"), err)
		return upload, nil
	}
	if !info.HasVideo || (info.HasAudio && item.Kind != "animation") {
		return upload, nil
	}
	if !info.HasAudio && info.VideoCodec == "h264" && info.PixelFormat == "yuv420p" &&
		strings.Contains(info.FormatName, "mp4") && strings.EqualFold(filepath.Ext(item.Name), ".mp4") {
		upload.Kind = "animation"
		return upload, nil
	}
	data, err := b.prepareVideoMP4(ctx, item.Data, item.Name, true)
	if err != nil {
		if ctx.Err() != nil {
			return upload, ctx.Err()
		}
		log.Printf(resources.Get("telegram.log.animation_prepare_failed"), err)
		return upload, nil
	}
	upload.Kind, upload.MIME, upload.Data, upload.URL = "animation", "video/mp4", data, ""
	upload.Name = strings.TrimSuffix(item.Name, filepath.Ext(item.Name)) + ".mp4"
	return upload, nil
}

// A timeout or lost response may follow a successful upload. Change the media
// type only after a definite rejection, so the fallback cannot duplicate it.
func mediaUploadRejected(err error) bool {
	var apiErr *telegram.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode == 400 || apiErr.StatusCode == 400)
}
