package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestDownloadedUploadDetectsSilentMP4WithoutChangingBytes(t *testing.T) {
	for _, audio := range []bool{false, true} {
		name, wantKind := "silent", "animation"
		if audio {
			name, wantKind = "with audio", "video"
		}
		t.Run(name, func(t *testing.T) {
			data := downloadedAnimationFixture(t, ".mp4", audio)
			b := &Bot{}
			item := downloader.Media{Kind: "video", Name: "source.mp4", MIME: "video/mp4", Data: data}
			upload, err := b.downloadedUpload(context.Background(), item)
			if err != nil || upload.Kind != wantKind || upload.Name != item.Name || upload.MIME != item.MIME || !bytes.Equal(upload.Data, data) {
				t.Fatalf("presentation changed bytes or selected wrong type: kind=%q name=%q MIME=%q identical=%t err=%v", upload.Kind, upload.Name, upload.MIME, bytes.Equal(upload.Data, data), err)
			}
		})
	}
}

func TestDownloadedUploadPreservesMediaWhenProbeUnavailable(t *testing.T) {
	b := &Bot{cfg: config.Config{FFprobePath: filepath.Join(t.TempDir(), "missing-ffprobe")}}
	for _, kind := range []string{"video", "animation"} {
		t.Run(kind, func(t *testing.T) {
			item := downloader.Media{Kind: kind, Name: "source.mp4", MIME: "video/mp4", Data: []byte("original media bytes")}
			upload, err := b.downloadedUpload(context.Background(), item)
			if err != nil || upload.Kind != kind || !bytes.Equal(upload.Data, item.Data) {
				t.Fatalf("probe failure must preserve known media type and bytes: kind=%q err=%v", upload.Kind, err)
			}
		})
	}
}

func TestDownloadedUploadInvalidMediaRemainsVideo(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	b := &Bot{}
	data := []byte("not a video")
	upload, err := b.downloadedUpload(context.Background(), downloader.Media{Kind: "video", Name: "broken.mp4", MIME: "video/mp4", Data: data})
	if err != nil || upload.Kind != "video" || !bytes.Equal(upload.Data, data) {
		t.Fatalf("unreadable media was classified as silent: kind=%q err=%v", upload.Kind, err)
	}
}

func TestDownloadedUploadConvertsSilentWebMAndPreservesSourceOnFailure(t *testing.T) {
	data := downloadedAnimationFixture(t, ".webm", false)
	item := downloader.Media{Kind: "video", Name: "source.webm", MIME: "video/webm", Data: data}
	for _, fail := range []bool{false, true} {
		name := "conversion succeeds"
		if fail {
			name = "encoder unavailable"
		}
		t.Run(name, func(t *testing.T) {
			b := &Bot{}
			if fail {
				b.cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
			}
			upload, err := b.downloadedUpload(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if upload.Kind != "video" || upload.Name != item.Name || upload.MIME != item.MIME || !bytes.Equal(upload.Data, data) {
					t.Fatal("failed conversion must preserve the original video")
				}
				return
			}
			if upload.Kind != "animation" || upload.Name != "source.mp4" || upload.MIME != "video/mp4" {
				t.Fatalf("unexpected converted presentation: kind=%q name=%q MIME=%q", upload.Kind, upload.Name, upload.MIME)
			}
			info, err := b.probeVideo(context.Background(), upload.Data, upload.Name)
			if err != nil || !info.HasVideo || info.HasAudio || info.VideoCodec != "h264" || !strings.Contains(info.FormatName, "mp4") {
				t.Fatalf("converted file is not a silent H.264 MP4: %+v, %v", info, err)
			}
		})
	}
}

func TestDownloadedAnimationSplitsAlbumsAndKeepsReplyCache(t *testing.T) {
	b := downloadedAnimationTestBot(t)
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: -100, Type: "supergroup"}}
	result := downloader.Result{Caption: "Post text\n\nAuthor", Items: []downloader.Media{
		{Kind: "photo", Name: "first.jpg", MIME: "image/jpeg", Data: []byte("first")},
		{Kind: "photo", Name: "second.jpg", MIME: "image/jpeg", Data: []byte("second")},
		{Kind: "animation", Name: "third.gif", MIME: "image/gif", Data: []byte("GIF89a")},
		{Kind: "photo", Name: "fourth.jpg", MIME: "image/jpeg", Data: []byte("fourth")},
		{Kind: "video", Name: "fifth.mp4", MIME: "video/mp4", Data: []byte("fifth")},
	}}
	var methods, captions []string
	var delivered []byte
	nextID := 100
	downloadedAnimationTransport(t, func(r *http.Request) (*http.Response, error) {
		method := filepath.Base(r.URL.Path)
		methods = append(methods, method)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		var reply struct {
			MessageID int `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != message.MessageID || r.FormValue("chat_id") != "-100" {
			t.Fatalf("incorrect media destination: reply=%+v chat=%q err=%v", reply, r.FormValue("chat_id"), err)
		}
		var sent []telegram.Message
		switch method {
		case "sendMediaGroup":
			var media []struct{ Type, Media, Caption string }
			if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil || len(media) != 2 {
				t.Fatalf("expected two-item album: %+v, %v", media, err)
			}
			for _, item := range media {
				if item.Type != "photo" && item.Type != "video" {
					t.Fatalf("animation entered album: %+v", item)
				}
				captions = append(captions, item.Caption)
				delivered = append(delivered, downloadedAnimationPart(t, r, strings.TrimPrefix(item.Media, "attach://"))...)
				sent = append(sent, telegram.Message{MessageID: nextID})
				nextID++
			}
		case "sendAnimation":
			captions = append(captions, r.FormValue("caption"))
			delivered = append(delivered, downloadedAnimationPart(t, r, "animation")...)
			sent = append(sent, telegram.Message{MessageID: nextID})
			nextID++
		default:
			t.Fatalf("unexpected method: %s", method)
		}
		if len(sent) == 1 {
			return downloadedAnimationResponse(t, sent[0]), nil
		}
		return downloadedAnimationResponse(t, sent), nil
	})
	sent, err := b.sendDownloadedMedia(context.Background(), message, result)
	if err != nil || len(sent) != len(result.Items) {
		t.Fatalf("delivery=%v err=%v", sent, err)
	}
	if !reflect.DeepEqual(methods, []string{"sendMediaGroup", "sendAnimation", "sendMediaGroup"}) {
		t.Fatalf("mixed media sequence=%v", methods)
	}
	if !reflect.DeepEqual(captions, []string{result.Caption, "", "", "", ""}) || string(delivered) != "firstsecondGIF89afourthfifth" {
		t.Fatalf("media order or unique caption lost: captions=%q bytes=%q", captions, delivered)
	}
	for index, item := range result.Items {
		cached := b.media.Get(message.Chat.ID, 100+index)
		if len(cached) != 1 || cached[0].Kind != item.Kind || cached[0].Name != item.Name || !bytes.Equal(cached[0].Data, item.Data) {
			t.Fatalf("message %d cached the wrong original attachment", 100+index)
		}
		reply := &telegram.Message{Chat: message.Chat, Sticker: &telegram.Sticker{FileID: "sticker"}, ReplyToMessage: &telegram.Message{MessageID: 100 + index, From: &b.identity}}
		if b.shouldHandleGroupLLM(reply) {
			t.Fatalf("sticker reply to downloaded message %d triggered the LLM", 100+index)
		}
	}
}

func TestDownloadedAnimationFallsBackToVideoOnlyAfterRejection(t *testing.T) {
	b := downloadedAnimationTestBot(t)
	item := downloader.Media{Kind: "animation", Name: "source.mp4", MIME: "video/mp4", Data: []byte("original mp4")}
	var methods []string
	downloadedAnimationTransport(t, func(r *http.Request) (*http.Response, error) {
		method := filepath.Base(r.URL.Path)
		methods = append(methods, method)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		field := "animation"
		if method == "sendVideo" {
			field = "video"
		} else if method != "sendAnimation" {
			t.Fatalf("unexpected fallback method: %s", method)
		}
		if !bytes.Equal(downloadedAnimationPart(t, r, field), item.Data) || r.FormValue("caption") != "Caption" {
			t.Fatal("fallback changed the file or dropped its caption")
		}
		if method == "sendAnimation" {
			return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"animation rejected"}`))}, nil
		}
		return downloadedAnimationResponse(t, telegram.Message{MessageID: 100}), nil
	})
	sent, err := b.sendDownloadedMedia(context.Background(), &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1}}, downloader.Result{Items: []downloader.Media{item}, Caption: "Caption"})
	if err != nil || len(sent) != 1 || !reflect.DeepEqual(methods, []string{"sendAnimation", "sendVideo"}) {
		t.Fatalf("fallback delivery=%v methods=%v err=%v", sent, methods, err)
	}
	cached := b.media.Get(1, 100)
	if len(cached) != 1 || !bytes.Equal(cached[0].Data, item.Data) {
		t.Fatal("fallback did not preserve the original media in the reply cache")
	}
}

func TestDownloadedAnimationAmbiguousFailureDoesNotResendOrCacheUndeliveredMedia(t *testing.T) {
	for _, album := range []bool{false, true} {
		name := "animation"
		if album {
			name = "album"
		}
		t.Run(name, func(t *testing.T) {
			b := downloadedAnimationTestBot(t)
			items := []downloader.Media{
				{Kind: "animation", Name: "first.gif", MIME: "image/gif", Data: []byte("first")},
				{Kind: "animation", Name: "failed.mp4", MIME: "video/mp4", Data: []byte("failed")},
				{Kind: "photo", Name: "unsent.jpg", MIME: "image/jpeg", Data: []byte("unsent")},
			}
			if album {
				items[1].Kind = "video"
			}
			var methods []string
			downloadedAnimationTransport(t, func(r *http.Request) (*http.Response, error) {
				methods = append(methods, filepath.Base(r.URL.Path))
				if len(methods) == 1 {
					return downloadedAnimationResponse(t, telegram.Message{MessageID: 100}), nil
				}
				// The server may have accepted the media, but its result was lost.
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":"lost confirmation"}`))}, nil
			})
			sent, err := b.sendDownloadedMedia(context.Background(), &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1}}, downloader.Result{Items: items})
			wantMethod := "sendAnimation"
			if album {
				wantMethod = "sendMediaGroup"
			}
			if err == nil || len(sent) != 1 || !reflect.DeepEqual(methods, []string{"sendAnimation", wantMethod}) {
				t.Fatalf("ambiguous delivery retried or lost confirmed message: sent=%v methods=%v err=%v", sent, methods, err)
			}
			cached := b.media.Get(1, 100)
			if len(cached) != 1 || !bytes.Equal(cached[0].Data, items[0].Data) || len(b.media.items) != 1 {
				t.Fatal("partial delivery must cache only the confirmed message")
			}
		})
	}
}

func downloadedAnimationFixture(t *testing.T, extension string, audio bool) []byte {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	output := filepath.Join(t.TempDir(), "fixture"+extension)
	arguments := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=size=64x48:rate=15"}
	if audio {
		arguments = append(arguments, "-f", "lavfi", "-i", "sine=frequency=440")
	}
	arguments = append(arguments, "-t", "0.2", "-threads", "1")
	if extension == ".webm" {
		arguments = append(arguments, "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8")
	} else {
		arguments = append(arguments, "-c:v", "libx264", "-pix_fmt", "yuv420p")
	}
	if audio {
		arguments = append(arguments, "-c:a", "aac")
	} else {
		arguments = append(arguments, "-an")
	}
	arguments = append(arguments, "-y", output)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "ffmpeg", arguments...).CombinedOutput(); err != nil {
		t.Fatalf("create video fixture: %v: %s", err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func downloadedAnimationTestBot(t *testing.T) *Bot {
	t.Helper()
	b := concurrentTestBot()
	b.cfg.FFprobePath = filepath.Join(t.TempDir(), "missing-ffprobe")
	b.identity = telegram.User{ID: 42, Username: "nyande_bot", IsBot: true}
	return b
}

func downloadedAnimationTransport(t *testing.T, transport moderationTransport) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = transport
}

func downloadedAnimationPart(t *testing.T, request *http.Request, field string) []byte {
	t.Helper()
	file, _, err := request.FormFile(field)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func downloadedAnimationResponse(t *testing.T, result any) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}
