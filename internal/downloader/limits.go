package downloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxMediaResultBytes = 100 * 1024 * 1024

var errMediaResultTooLarge = errors.New("media collection exceeds the total download size limit")

// Keep at most 100 MiB per collection unless configuration explicitly permits a
// larger individual file. Peak media storage is bounded by this plus one item.
func (d *Downloader) appendMedia(items []Media, item Media) ([]Media, error) {
	remaining := max(int64(maxMediaResultBytes), d.cfg.MaxFileSize)
	for _, existing := range items {
		remaining -= int64(len(existing.Data)) + int64(len(existing.Thumbnail))
	}
	if int64(len(item.Data))+int64(len(item.Thumbnail)) > remaining {
		return nil, errMediaResultTooLarge
	}
	return append(items, item), nil
}

func terminalDownloadError(err error) bool {
	return errors.Is(err, ErrVideoCompression) || errors.Is(err, errMediaResultTooLarge)
}

// Read before decoding so an oversized valid JSON prefix cannot bypass the cap.
func decodeLimitedJSON(body io.Reader, limit int64, target any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("JSON response exceeds %d bytes", limit)
	}
	return json.Unmarshal(data, target)
}
