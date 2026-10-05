package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestLLMCreateFileDeliversExactCode(t *testing.T) {
	const content = "\n# Привет\r\nconfig = {\"path\": \"C:\\\\tmp\\\\кот\", \"quote\": \"\\\"yes\\\"\"}\nprint(\"мяу\")\n\n"
	for _, test := range []struct{ mode, filename string }{
		{"native", "программа.py"},
		{"json text fallback", "программа.py"},
		{"native", "Main.java"},
		{"json text fallback", "main.cpp"},
		{"native", "Program.cs"},
		{"json text fallback", "Main.kt"},
		{"native", "script.kts"},
		{"json text fallback", "code.custom"},
		{"native", "Makefile"},
	} {
		t.Run(test.mode+"/"+test.filename, func(t *testing.T) {
			requests, uploads := 0, 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":{"message_id":99}}`
				if r.URL.Hostname() == "api.telegram.org" {
					uploads++
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sendDocument") {
						t.Fatalf("unexpected Telegram request: %s %s", r.Method, r.URL.Path)
					}
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					file, header, err := r.FormFile("document")
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(file)
					file.Close()
					if err != nil || string(data) != content || header.Filename != test.filename || !strings.HasPrefix(header.Header.Get("Content-Type"), "text/plain") {
						t.Fatalf("attachment changed: filename=%q content=%q err=%v", header.Filename, data, err)
					}
					var reply struct {
						MessageID int `json:"message_id"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != 17 || r.FormValue("chat_id") != "-100" {
						t.Fatalf("wrong delivery scope: chat=%q reply=%#v err=%v", r.FormValue("chat_id"), reply, err)
					}
				} else {
					if r.URL.Hostname() != "llm.test" {
						t.Fatalf("unexpected network destination: %s", r.URL.Hostname())
					}
					requests++
					var payload struct {
						Messages []struct{ Role, Content string }
						Tools    []struct{ Function struct{ Name string } }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					assistant := map[string]any{"content": "Файл отправлен."}
					if requests == 1 {
						advertised := false
						for _, tool := range payload.Tools {
							advertised = advertised || tool.Function.Name == "create_file"
						}
						if !advertised || uploads != 0 {
							t.Fatal("create_file was not advertised before delivery")
						}
						args := map[string]string{"filename": " " + test.filename + " ", "content": content, "chat_id": "999"}
						encoded, _ := json.Marshal(args)
						assistant = map[string]any{"tool_calls": []any{map[string]any{"id": "file-1", "type": "function", "function": map[string]any{"name": "create_file", "arguments": string(encoded)}}}}
						if test.mode == "json text fallback" {
							encoded, _ = json.Marshal(map[string]any{"name": "create_file", "arguments": args})
							assistant = map[string]any{"content": "<tool_call>" + string(encoded) + "</tool_call>"}
						}
					} else {
						last := payload.Messages[len(payload.Messages)-1]
						if uploads != 1 || last.Role != "tool" || !strings.Contains(last.Content, "Successfully sent") {
							t.Fatalf("missing confirmed delivery observation: %#v uploads=%d", last, uploads)
						}
					}
					encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": assistant}}})
					body = string(encoded)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			b := &Bot{telegram: telegram.New("test-token")}
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: -100}}
			client := llm.New(config.Config{LLMEnabled: true, LLMBaseURL: "https://llm.test", LLMModel: "test", LLMTimeout: time.Second})
			answer, err := client.Ask(context.Background(), llm.Request{ChatID: -100, Text: "Напиши скрипт и пришли файлом", Tools: []llm.Tool{b.createFileTool(message)}})
			if err != nil || answer != "Файл отправлен." || requests != 2 || uploads != 1 {
				t.Fatalf("answer=%q err=%v requests=%d uploads=%d", answer, err, requests, uploads)
			}
		})
	}
}

func TestCreateFileValidatesNamesAndByteLimits(t *testing.T) {
	uploads := 0
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		uploads++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Header: make(http.Header)}, nil
	})
	b := &Bot{telegram: telegram.New("test-token")}
	tool := b.createFileTool(&telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 100}})
	for _, filename := range []string{"", " ", ".", "..", "../a.py", "dir/a.py", `dir\a.py`, `a".py`, "bad\x00.py", "bad\n.py", "bad\x7f.py", "bad\u0085.py", "\xff", strings.Repeat("a", 201), strings.Repeat("я", 101)} {
		if result, err := tool.Execute(context.Background(), map[string]string{"filename": filename, "content": "code"}); err != nil || result == "" || uploads != 0 {
			t.Fatalf("invalid filename %q: result=%q err=%v uploads=%d", filename, result, err, uploads)
		}
	}
	for _, content := range []string{"", "\xff", strings.Repeat("я", maxGeneratedFileBytes/2+1)} {
		if result, err := tool.Execute(context.Background(), map[string]string{"filename": "code.py", "content": content}); err != nil || result == "" || uploads != 0 {
			t.Fatalf("invalid content length=%d: result=%q err=%v uploads=%d", len(content), result, err, uploads)
		}
	}
	result, err := tool.Execute(context.Background(), map[string]string{"filename": strings.Repeat("я", 100), "content": strings.Repeat("я", maxGeneratedFileBytes/2)})
	if err != nil || !strings.Contains(result, "Successfully sent") || uploads != 1 {
		t.Fatalf("exact byte limits rejected: result=%q err=%v uploads=%d", result, err, uploads)
	}
	if result, err := tool.Execute(context.Background(), map[string]string{"filename": "blank.txt", "content": " \t\n"}); err != nil || !strings.Contains(result, "Successfully sent") || uploads != 2 {
		t.Fatalf("whitespace-only content rejected: result=%q err=%v uploads=%d", result, err, uploads)
	}
}

func TestCreateFileDeduplicatesAttemptsAndHonorsCancellation(t *testing.T) {
	uploads := 0
	ambiguous := true
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		uploads++
		body := `{"ok":true,"result":{"message_id":1}}`
		if ambiguous {
			body = `{"ok":true,"result":"malformed confirmation"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	b := &Bot{telegram: telegram.New("test-token")}
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 100}}
	tool := b.createFileTool(message)
	args := map[string]string{"filename": "first.py", "content": "code"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, args); !errors.Is(err, context.Canceled) || uploads != 0 {
		t.Fatalf("cancelled request attempted delivery: err=%v uploads=%d", err, uploads)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(result, "not confirmed") || uploads != 1 {
		t.Fatalf("ambiguous delivery result=%q err=%v uploads=%d", result, err, uploads)
	}
	ambiguous = false
	args["filename"], args["content"] = " first.py ", "different code"
	if result, err := tool.Execute(context.Background(), args); err != nil || !strings.Contains(result, "not confirmed") || uploads != 1 {
		t.Fatalf("ambiguous failure resent: result=%q err=%v uploads=%d", result, err, uploads)
	}
	for index := 1; index < maxGeneratedFiles; index++ {
		args["filename"] = fmt.Sprintf("file%d.py", index)
		if result, err := tool.Execute(context.Background(), args); err != nil || !strings.Contains(result, "Successfully sent") {
			t.Fatalf("valid attempt %d failed: result=%q err=%v", index, result, err)
		}
	}
	args["content"] = "changed code with the same filename"
	if _, err := tool.Execute(context.Background(), args); err != nil || uploads != maxGeneratedFiles {
		t.Fatalf("successful file resent: err=%v uploads=%d", err, uploads)
	}
	args["filename"] = "over-limit.py"
	if result, err := tool.Execute(context.Background(), args); err != nil || result == "" || strings.Contains(result, "Successfully sent") || uploads != maxGeneratedFiles {
		t.Fatalf("attempt limit ignored: result=%q err=%v uploads=%d", result, err, uploads)
	}
	args["filename"] = "file1.py"
	if result, err := b.createFileTool(message).Execute(context.Background(), args); err != nil || !strings.Contains(result, "Successfully sent") || uploads != maxGeneratedFiles+1 {
		t.Fatalf("new request inherited attempts: result=%q err=%v uploads=%d", result, err, uploads)
	}
}
