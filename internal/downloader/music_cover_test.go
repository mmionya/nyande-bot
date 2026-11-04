package downloader

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/music"
)

func coverPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width/2; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: 220, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestMusicCoversBecomeBoundedJPEGs(t *testing.T) {
	large := coverPNG(t, 1600, 800)
	decoded, _, err := image.Decode(bytes.NewReader(large))
	if err != nil {
		t.Fatal(err)
	}
	var jpegSource bytes.Buffer
	if err := jpeg.Encode(&jpegSource, decoded, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRjwAAABXRUJQVlA4IDAAAADQAQCdASoQABAAAgA0JaACdLoB+AADsAD+8Oj3/yC5YXXI1/8gP+QH/ID/+PIAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		name          string
		data          []byte
		width, height int
	}{
		{"png", large, 320, 160}, {"jpeg", jpegSource.Bytes(), 320, 160}, {"webp", webp, 16, 16},
	} {
		t.Run(input.name, func(t *testing.T) {
			cover, thumb, err := normalizeMusicCover(input.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				data        []byte
				side, limit int
			}{{cover, 1024, 1024 * 1024}, {thumb, 320, 200000}} {
				im, format, err := image.Decode(bytes.NewReader(item.data))
				if err != nil || format != "jpeg" || len(item.data) >= item.limit || im.Bounds().Dx() > item.side || im.Bounds().Dy() > item.side {
					t.Fatalf("bad cover: format=%s bytes=%d err=%v", format, len(item.data), err)
				}
			}
			im, _, _ := image.Decode(bytes.NewReader(thumb))
			if im.Bounds().Dx() != input.width || im.Bounds().Dy() != input.height {
				t.Fatal("cover aspect ratio changed")
			}
			if input.name == "png" {
				r, g, b, _ := im.At(300, 100).RGBA()
				if r < 60000 || g < 60000 || b < 60000 {
					t.Fatal("transparent cover did not receive a white background")
				}
			}
		})
	}
}

func TestMusicCoverRejectsLargeImagesBeforeDecoding(t *testing.T) {
	var header bytes.Buffer
	header.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&header, binary.BigEndian, uint32(13))
	payload := append([]byte("IHDR"), make([]byte, 13)...)
	binary.BigEndian.PutUint32(payload[4:8], 10000)
	binary.BigEndian.PutUint32(payload[8:12], 10000)
	payload[12], payload[13] = 8, 2
	header.Write(payload)
	_ = binary.Write(&header, binary.BigEndian, crc32.ChecksumIEEE(payload))
	if _, _, err := normalizeMusicCover(header.Bytes()); err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("large image was not rejected before decode: %v", err)
	}
	if _, _, err := normalizeMusicCover(make([]byte, musicCoverDownloadLimit+1)); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatal("oversized bytes accepted")
	}
	if _, _, err := normalizeMusicCover([]byte("<html>Not an image</html>")); err == nil {
		t.Fatal("error page accepted as artwork")
	}
}

func TestMusicCoverPrefersFullMetadataAndFallsBack(t *testing.T) {
	imageData := coverPNG(t, 64, 32)
	for _, mode := range []string{"metadata", "thumbnail list", "search only", "metadata failure", "invalid image", "all unavailable", "private URL", "private redirect", "oversized response", "unknown length"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			metadata := map[string]any{"thumbnail": "https://8.8.8.8/full.png"}
			if mode == "thumbnail list" {
				metadata = map[string]any{"thumbnails": []map[string]string{{"url": "https://8.8.8.8/small.png"}, {"url": "https://8.8.8.8/full.png"}}}
			}
			if mode == "search only" {
				metadata = map[string]any{}
			}
			fallback := "https://8.8.4.4/search.png"
			if mode == "private URL" {
				metadata = map[string]any{"thumbnail": "https://127.0.0.1/private.png"}
				fallback = ""
			}
			if mode == "private redirect" || mode == "oversized response" || mode == "unknown length" {
				fallback = ""
			}
			data, _ := json.Marshal(metadata)
			if err := os.WriteFile(filepath.Join(dir, "track.info.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var requested []string
			d := New(config.Config{})
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				requested = append(requested, r.URL.String())
				if r.URL.Hostname() == "127.0.0.1" {
					t.Fatal("connected to private cover URL")
				}
				body := imageData
				status := http.StatusOK
				headers := http.Header{"Content-Type": {"image/png"}}
				length := int64(len(body))
				if mode == "all unavailable" || (mode == "metadata failure" && r.URL.Path == "/full.png") {
					status = 404
				}
				if mode == "invalid image" && r.URL.Path == "/full.png" {
					body = []byte("invalid")
					length = int64(len(body))
				}
				if mode == "private redirect" {
					status = 302
					headers.Set("Location", "http://127.0.0.1/private.png")
				}
				if mode == "oversized response" {
					length = musicCoverDownloadLimit + 1
				}
				if mode == "unknown length" {
					body = make([]byte, musicCoverDownloadLimit+1)
					length = -1
				}
				return &http.Response{StatusCode: status, Header: headers, ContentLength: length, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
			})
			cover, thumb := d.musicCover(context.Background(), dir, fallback)
			missing := mode == "all unavailable" || mode == "private URL" || mode == "private redirect" || mode == "oversized response" || mode == "unknown length"
			if missing {
				if len(cover) != 0 || len(thumb) != 0 {
					t.Fatal("invalid cover was returned")
				}
			} else if len(cover) == 0 || len(thumb) == 0 {
				t.Fatal("cover was not downloaded")
			}
			if mode == "metadata" || mode == "thumbnail list" {
				if len(requested) != 1 || requested[0] != "https://8.8.8.8/full.png" {
					t.Fatalf("wrong preferred image: %v", requested)
				}
			}
			if mode == "metadata failure" || mode == "invalid image" {
				if len(requested) != 2 || requested[1] != fallback {
					t.Fatal("search cover fallback not used")
				}
			}
			if mode == "private URL" && len(requested) != 0 {
				t.Fatal("private cover was requested")
			}
			if mode == "private redirect" && len(requested) != 1 {
				t.Fatal("private redirect was followed")
			}
		})
	}
}

func TestMusicDownloadEmbedsCoverAndPreservesAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	original := filepath.Join(dir, "original.mp3")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.3", "-c:a", "libmp3lame", original).CombinedOutput(); err != nil {
		t.Fatalf("test audio: %v %s", err, out)
	}
	originalData, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	cover := coverPNG(t, 640, 480)
	for _, mode := range []string{"embedded", "cover unavailable", "converter failed", "size cap"} {
		t.Run(mode, func(t *testing.T) {
			script := fakeYTDLPProcess(t, `output=''
for arg in "$@"; do
 if [ "$output" = next ]; then output="$arg"; break; fi
 if [ "$arg" = --output ]; then output=next; fi
done
cp "$0.mp3" "${output%.*}.mp3"
printf '%s' '{"thumbnail":"https://8.8.8.8/cover.png"}' > "${output%.*}.info.json"
printf '%s' "${output%/*}" > "$0.directory"
`)
			if err := os.WriteFile(script+".mp3", originalData, 0600); err != nil {
				t.Fatal(err)
			}
			converter := ffmpeg
			limit := int64(1024 * 1024)
			if mode == "converter failed" {
				converter = fakeYTDLPProcess(t, "exit 1")
			}
			if mode == "size cap" {
				limit = int64(len(originalData)) + 1
			}
			d := New(config.Config{YTDownloadPath: script, FFmpegPath: converter, MaxFileSize: limit})
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				status := 200
				if mode == "cover unavailable" {
					status = 404
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"image/png"}}, ContentLength: int64(len(cover)), Body: io.NopCloser(bytes.NewReader(cover)), Request: r}, nil
			})
			result, err := d.DownloadMusic(context.Background(), music.Track{URL: "https://youtu.be/song"})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cover unavailable" {
				if len(result.Thumbnail) != 0 {
					t.Fatal("unavailable cover was attached")
				}
			} else if len(result.Thumbnail) == 0 {
				t.Fatal("Telegram thumbnail missing")
			}
			if mode != "embedded" {
				if !bytes.Equal(result.Data, originalData) {
					t.Fatal("cover failure or size cap changed the original audio")
				}
			} else {
				path := filepath.Join(t.TempDir(), "received.mp3")
				if err := os.WriteFile(path, result.Data, 0600); err != nil {
					t.Fatal(err)
				}
				probe, err := exec.Command(ffprobe, "-v", "error", "-show_streams", "-of", "json", path).Output()
				if err != nil {
					t.Fatal(err)
				}
				var parsed struct {
					Streams []struct {
						CodecType   string `json:"codec_type"`
						CodecName   string `json:"codec_name"`
						Disposition struct {
							Attached int `json:"attached_pic"`
						} `json:"disposition"`
					}
				}
				if err := json.Unmarshal(probe, &parsed); err != nil {
					t.Fatal(err)
				}
				audio, picture := false, false
				for _, stream := range parsed.Streams {
					audio = audio || stream.CodecType == "audio" && stream.CodecName == "mp3"
					picture = picture || stream.CodecName == "mjpeg" && stream.Disposition.Attached == 1
				}
				if !audio || !picture {
					t.Fatalf("MP3 is missing audio or attached artwork: %s", probe)
				}
				hash := func(file string) []byte {
					data, err := exec.Command(ffmpeg, "-v", "error", "-i", file, "-map", "0:a:0", "-c:a", "copy", "-f", "hash", "-hash", "sha256", "-").Output()
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
				if !bytes.Equal(hash(original), hash(path)) {
					t.Fatal("embedding changed compressed audio frames")
				}
			}
			work, err := os.ReadFile(script + ".directory")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(string(work)); !os.IsNotExist(err) {
				t.Fatal("temporary audio/artwork files were retained")
			}
		})
	}
}
