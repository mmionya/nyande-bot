package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/gif"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/resources"
)

func TestGIFSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message *discordgo.Message
		want    string
	}{
		{"missing", nil, ""},
		{"usage", &discordgo.Message{Content: "!gif"}, ""},
		{"link", &discordgo.Message{Content: "!gif https://example.com/video.mp4"}, "https://example.com/video.mp4"},
		{"post", &discordgo.Message{Content: "!gif https://www.youtube.com/watch?v=example"}, "https://www.youtube.com/watch?v=example"},
		{"attachment", &discordgo.Message{Content: "!gif https://example.com/other.mp4", Attachments: []*discordgo.MessageAttachment{nil, {URL: "https://cdn.discordapp.com/attachments/video.mp4?ex=123"}}}, "https://cdn.discordapp.com/attachments/video.mp4?ex=123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gifSource(tc.message); got != tc.want {
				t.Fatalf("gifSource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGIFCommandUsageAndReplyLookup(t *testing.T) {
	for _, fetchReply := range []bool{false, true} {
		b, err := New(config.Config{DiscordToken: "test-token", DiscordPrefix: "?"})
		if err != nil {
			t.Fatal(err)
		}
		message := &discordgo.Message{ID: "command", ChannelID: "channel", Content: "?gif"}
		if fetchReply {
			message.MessageReference = &discordgo.MessageReference{MessageID: "original", ChannelID: "channel"}
		}
		gets, posts := 0, 0
		b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"id":"reply"}`
			if req.Method == http.MethodGet {
				gets++
				body = `{"id":"original","content":"no video"}`
			} else {
				posts++
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), resources.Format("discord_gif_usage", map[string]any{"prefix": "?"})) {
					t.Fatalf("missing usage with configured prefix: %s", data)
				}
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if !b.handleCommand(b.session, message, "gif") || b.commands.Load() != 1 || posts != 1 {
			t.Fatal("gif command was not handled once")
		}
		if (gets == 1) != fetchReply {
			t.Fatalf("reply requests = %d", gets)
		}
		if b.mediaTotal.Load() != 0 {
			t.Fatal("usage counted as media")
		}
	}
}

func TestConvertGIF(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	input := filepath.Join(t.TempDir(), "video.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=15", "-t", "0.4", "-threads", "1", "-pix_fmt", "yuv420p", "-y", input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create video: %v: %s", err, out)
	}
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("reply upload", func(t *testing.T) { testGIFReplyUpload(t, data) })
	t.Run("animated GIF", func(t *testing.T) {
		out, err := convertGIF(context.Background(), data, 1024*1024)
		if err != nil {
			t.Fatal(err)
		}
		animation, err := gif.DecodeAll(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if len(animation.Image) < 2 || animation.Config.Width != 64 || animation.Config.Height != 48 || animation.LoopCount != 0 {
			t.Fatalf("unexpected animation: frames=%d, size=%dx%d, loop=%d", len(animation.Image), animation.Config.Width, animation.Config.Height, animation.LoopCount)
		}
	})
	t.Run("input too large", func(t *testing.T) {
		if _, err := convertGIF(context.Background(), data, 1); !errors.Is(err, errGIFTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := convertGIF(ctx, data, 1024*1024); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("invalid video", func(t *testing.T) {
		if _, err := convertGIF(context.Background(), []byte("not a video"), 1024*1024); err == nil {
			t.Fatal("expected conversion failure")
		}
	})
}

func testGIFReplyUpload(t *testing.T, video []byte) {
	t.Helper()
	b, err := New(config.Config{DiscordToken: "test-token", DiscordPrefix: "!", MaxFileSize: 1024 * 1024, MaxMediaItems: 10, RetryAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	uploads, deletes := 0, 0
	transport := discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req}
		if req.URL.Host == "8.8.8.8" {
			response.Header.Set("Content-Type", "video/mp4")
			response.Body = io.NopCloser(bytes.NewReader(video))
			return response, nil
		}
		if strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/form-data") {
			uploads++
			if err := req.ParseMultipartForm(2 * 1024 * 1024); err != nil {
				t.Fatal(err)
			}
			defer req.MultipartForm.RemoveAll()
			files := req.MultipartForm.File["files[0]"]
			if len(files) != 1 || files[0].Filename != "animation.gif" {
				t.Fatalf("files = %#v", req.MultipartForm.File)
			}
			file, err := files[0].Open()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := gif.DecodeAll(file); err != nil {
				t.Fatalf("uploaded file is not GIF: %v", err)
			}
		} else if req.Method == http.MethodDelete {
			deletes++
		} else {
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			statuses = append(statuses, payload.Content)
		}
		response.Body = io.NopCloser(strings.NewReader(`{"id":"status"}`))
		return response, nil
	})
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
	b.session.Client = &http.Client{Transport: transport}
	message := &discordgo.Message{ID: "command", ChannelID: "channel", Content: "!gif", ReferencedMessage: &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{URL: "https://8.8.8.8/video.mp4"}}}}
	if !b.handleCommand(b.session, message, "gif") {
		t.Fatal("command not handled")
	}
	want := []string{resources.Get("discord_downloading"), resources.Get("discord_gif_converting"), resources.Get("discord_sending")}
	if !reflect.DeepEqual(statuses, want) || uploads != 1 || deletes != 1 || b.mediaErrors.Load() != 0 || b.mediaTotal.Load() != 1 || b.commands.Load() != 1 {
		t.Fatalf("statuses=%q, uploads=%d, deletes=%d, errors=%d, total=%d, commands=%d", statuses, uploads, deletes, b.mediaErrors.Load(), b.mediaTotal.Load(), b.commands.Load())
	}
}
