package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestAudioCommandReportsMissingAndUnavailableMedia(t *testing.T) {
	for _, mode := range []string{"missing", "photo", "cached too large", "Telegram too large", "download failed", "encoder missing"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "/audio"}
			want := resources.Get("telegram.audio.usage")
			switch mode {
			case "photo", "cached too large", "encoder missing":
				message.ReplyToMessage = &telegram.Message{MessageID: 16}
				attachment := cachedAttachment{Kind: "audio", Name: "sound.wav", MIME: "audio/wav", Data: []byte("audio")}
				if mode == "photo" {
					attachment.Kind = "photo"
				} else if mode == "cached too large" {
					b.cfg.MaxFileSize = 2
					want = resources.Get("telegram.audio.too_large")
				} else {
					want = strings.Split(resources.Get("telegram.audio.convert_failed"), "%v")[0]
				}
				b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{attachment})
			case "Telegram too large":
				message.ReplyToMessage = &telegram.Message{Voice: &telegram.Voice{FileID: "voice", FileSize: 21 << 20}}
				want = resources.Get("telegram.media.attachment_too_large")
			case "download failed":
				message.ReplyToMessage = &telegram.Message{Voice: &telegram.Voice{FileID: "voice"}}
				want = resources.Get("telegram.media.download_failed")
			}
			answers := 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":{"message_id":99}}`
				switch {
				case strings.HasSuffix(r.URL.Path, "/getFile") && mode == "download failed":
					body = `{"ok":false,"error_code":400,"description":"file unavailable"}`
				case strings.HasSuffix(r.URL.Path, "/sendMessage"):
					answers++
					var request struct {
						Text string
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if !strings.HasPrefix(request.Text, want) {
						t.Errorf("answer=%q, want %q", request.Text, want)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if answers != 1 {
				t.Fatalf("answers=%d, want 1", answers)
			}
		})
	}
}

func TestAudioCommandSendsMP3FromReplyAndCaption(t *testing.T) {
	video := gifTestVideo(t)
	for _, mode := range []string{"video", "animation", "video_note", "voice", "audio", "caption document", "send failed"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 2 << 20
			b.identity.Username = "NyandeBot"
			// Use the configured executable, independently of the default name.
			b.cfg.FFmpegPath, _ = exec.LookPath("ffmpeg")
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "/audio", ReplyToMessage: &telegram.Message{MessageID: 16}}
			kind := mode
			if mode == "send failed" {
				kind = "video"
			}
			b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{{Kind: kind, Name: "clip.mp4", MIME: "video/mp4", Data: video}})
			if mode == "caption document" {
				message.Text, message.Caption, message.ReplyToMessage = "", "/audio@NyandeBot", nil
				message.Document = &telegram.Document{FileID: "video", FileName: "clip.mp4", MimeType: "video/mp4", FileSize: int64(len(video))}
			}
			uploads, downloads, answers := 0, 0, 0
			var audio []byte
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":{"message_id":99}}`
				switch {
				case strings.HasSuffix(r.URL.Path, "/getFile") && mode == "caption document":
					body = `{"ok":true,"result":{"file_path":"clip.mp4"}}`
				case strings.Contains(r.URL.Path, "/file/") && mode == "caption document":
					downloads++
					body = string(video)
				case strings.HasSuffix(r.URL.Path, "/sendAudio"):
					uploads++
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					file, header, err := r.FormFile("audio")
					if err != nil {
						t.Fatal(err)
					}
					audio, err = io.ReadAll(file)
					file.Close()
					if err != nil || header.Filename != "clip.mp3" || header.Header.Get("Content-Type") != "audio/mpeg" {
						t.Fatalf("wrong audio upload: %v %v", header, err)
					}
					var reply struct {
						MessageID int `json:"message_id"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != 17 || r.FormValue("chat_id") != "1" {
						t.Fatalf("wrong destination: chat=%q reply=%#v err=%v", r.FormValue("chat_id"), reply, err)
					}
					if mode == "send failed" {
						body = `{"ok":false,"error_code":400,"description":"cannot send audio"}`
					}
				case strings.HasSuffix(r.URL.Path, "/sendMessage") && mode == "send failed":
					answers++
					var request struct{ Text string }
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if !strings.HasPrefix(request.Text, strings.Split(resources.Get("telegram.audio.send_failed"), "%v")[0]) {
						t.Fatal(request.Text)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			wantDownloads, wantAnswers := 0, 0
			if mode == "caption document" {
				wantDownloads = 1
			}
			if mode == "send failed" {
				wantAnswers = 1
			}
			if uploads != 1 || downloads != wantDownloads || answers != wantAnswers {
				t.Fatalf("uploads=%d downloads=%d answers=%d", uploads, downloads, answers)
			}
			path := filepath.Join(t.TempDir(), "clip.mp3")
			if err := os.WriteFile(path, audio, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_type", "-of", "csv=p=0", path).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "mp3,audio" {
				t.Fatalf("expected MP3 audio only: %s %v", out, err)
			}
		})
	}
}

func TestAudioConversionRejectsInvalidAndCanceledInput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	b := concurrentTestBot()
	b.cfg.MaxFileSize = 2 << 20
	if data, err := b.convertMediaToAudio(context.Background(), []byte("invalid media"), "bad.mp4"); err == nil || len(data) != 0 {
		t.Fatalf("invalid input returned %d bytes and %v", len(data), err)
	}
	silentVideo, err := convertVideoToGIF(context.Background(), gifTestVideo(t), "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := b.convertMediaToAudio(context.Background(), silentVideo, "silent.mp4"); err == nil || len(data) != 0 {
		t.Fatalf("video without audio returned %d bytes and %v", len(data), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := b.convertMediaToAudio(ctx, []byte("invalid media"), "bad.mp4"); !errors.Is(err, context.Canceled) || len(data) != 0 {
		t.Fatalf("canceled conversion returned %d bytes and %v", len(data), err)
	}
}

func TestAudioCommandRejectsOversizeConversion(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "silence.ogg")
	// Compressed silence is much smaller in Opus than in the outgoing MP3.
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "1", "-c:a", "libopus", "-y", path).CombinedOutput()
	if err != nil {
		t.Fatalf("create voice fixture: %v: %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := concurrentTestBot()
	b.cfg.MaxFileSize = int64(len(data) + 100)
	b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{{Kind: "voice", Name: "silence.ogg", MIME: "audio/ogg", Data: data}})
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	answers := 0
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("oversize conversion must not be uploaded: %s", r.URL)
		} else {
			answers++
			var request struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Text != resources.Get("telegram.audio.too_large") {
				t.Fatal(request.Text)
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`))}, nil
	})
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "/audio", ReplyToMessage: &telegram.Message{MessageID: 16}}
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if answers != 1 {
		t.Fatalf("answers=%d, want 1", answers)
	}
}
