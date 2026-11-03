package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func fakeWhisper(t *testing.T, transcript string, fail bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whisper")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$0.args"
input="$1"
shift
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '--output_dir' ]; then out="$2"; fi
  shift 2
done
cp "$input" "$0.input"
`
	if fail {
		script += "echo 'private recognized speech' >&2\nexit 1\n"
	} else {
		if err := os.WriteFile(path+".transcript", []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		script += "cp \"$0.transcript\" \"$out/input.txt\"\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVoiceTranscriptionArgumentsAndFailures(t *testing.T) {
	for _, mode := range []string{"success", "empty", "failed", "missing", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.WhisperLanguage = "ru"
			b.cfg.WhisperModel = "tiny"
			text := "Привет, как дела?"
			if mode == "empty" {
				text = " \n "
			}
			b.cfg.WhisperPath = fakeWhisper(t, text, mode == "failed")
			if mode == "missing" {
				b.cfg.WhisperPath = filepath.Join(t.TempDir(), "missing-whisper")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			got, err := b.transcribe(ctx, cachedAttachment{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("voice bytes")}, false)
			if mode != "success" {
				if err == nil || got != "" || strings.Contains(err.Error(), "private recognized speech") {
					t.Fatalf("invalid failure: %q %v", got, err)
				}
				if mode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || got != text {
				t.Fatalf("transcript=%q err=%v", got, err)
			}
			args, _ := os.ReadFile(b.cfg.WhisperPath + ".args")
			for _, expected := range []string{"--language\nru", "--model\ntiny", "--fp16\nFalse", "--verbose\nFalse", "--task\ntranscribe", "--output_format\ntxt"} {
				if !strings.Contains(string(args), expected) {
					t.Errorf("missing %q in %s", expected, args)
				}
			}
			data, _ := os.ReadFile(b.cfg.WhisperPath + ".input")
			if string(data) != "voice bytes" {
				t.Fatal("voice bytes not passed to Whisper")
			}
		})
	}
}

func TestTelegramVoiceMessages(t *testing.T) {
	for _, mode := range []string{"bare", "reply", "disabled", "failed", "empty", "download failure", "too large", "group unaddressed", "group reply"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.WhisperEnabled = mode != "disabled"
			b.cfg.MaxFileSize = 20 * 1024 * 1024
			b.identity.ID = 99
			transcript := "Какая погода сегодня?"
			if mode == "empty" {
				transcript = ""
			}
			b.cfg.WhisperPath = fakeWhisper(t, transcript, mode == "failed")
			message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 1, Type: "private"}, Voice: &telegram.Voice{FileID: "voice"}}
			if mode == "reply" {
				message = &telegram.Message{MessageID: 2, Chat: message.Chat, Text: "Ответь на голосовое", ReplyToMessage: message}
			}
			if mode == "too large" {
				message.Voice.FileSize = 21 * 1024 * 1024
			}
			if strings.HasPrefix(mode, "group") {
				message.Chat.Type = "supergroup"
				if mode == "group reply" {
					message.ReplyToMessage = &telegram.Message{MessageID: 3, From: &telegram.User{ID: 99}}
				}
			}
			calls, downloads := 0, 0
			var sent string
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":{"message_id":100}}`
				switch {
				case r.URL.Hostname() == "llm.test":
					calls++
					var payload struct {
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, transcript) {
						t.Error("transcript did not reach LLM")
					}
					body = `{"choices":[{"message":{"content":"Уточни город"}}]}`
				case strings.HasSuffix(r.URL.Path, "/getFile"):
					body = `{"ok":true,"result":{"file_path":"voice.ogg"}}`
					if mode == "download failure" {
						body = `{"ok":false,"error_code":400,"description":"file unavailable"}`
					}
				case strings.Contains(r.URL.Path, "/file/"):
					downloads++
					body = "voice bytes"
				case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
					body = `{"ok":true,"result":true}`
				case strings.HasSuffix(r.URL.Path, "/sendMessage"):
					var request struct{ Text string }
					json.NewDecoder(r.Body).Decode(&request)
					sent = request.Text
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err := b.handleLLM(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			want := 1
			switch mode {
			case "disabled":
				want = 0
				if sent != resources.Get("telegram.voice.disabled") {
					t.Fatal(sent)
				}
			case "failed", "empty":
				want = 0
				if sent != resources.Get("telegram.voice.failed") {
					t.Fatal(sent)
				}
			case "download failure", "too large":
				want = 0
				if sent != resources.Get("telegram.media.download_failed") {
					t.Fatal(sent)
				}
			case "group unaddressed":
				want = 0
				if sent != "" || downloads != 0 {
					t.Fatal("unaddressed voice processed")
				}
			default:
				if sent != "Уточни город" {
					t.Fatal(sent)
				}
			}
			if calls != want {
				t.Fatalf("LLM calls=%d want=%d", calls, want)
			}
			if mode == "too large" && downloads != 0 {
				t.Fatal("oversize voice downloaded")
			}
		})
	}
}
