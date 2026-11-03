package discordbot

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
)

func TestDiscordDocumentMessagesAndReplies(t *testing.T) {
	for _, mode := range []string{"bare", "caption", "reply", "fetch reply"} {
		t.Run(mode, func(t *testing.T) {
			b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "!", LLMVision: true})
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeLanguageModel{enabled: true, answer: "Summary"}
			b.llm = model
			data, err := os.ReadFile("../documents/testdata/sample.docx")
			if err != nil {
				t.Fatal(err)
			}
			b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := []byte(`{"id":"reply"}`)
				if r.URL.Hostname() == "cdn.discordapp.com" {
					if r.Header.Get("Authorization") != "" {
						t.Error("API credentials sent to CDN")
					}
					body = data
				} else if strings.HasSuffix(r.URL.Path, "/messages/parent") {
					body = []byte(`{"id":"parent","attachments":[{"filename":"sample.docx","url":"https://cdn.discordapp.com/attachments/1/2/sample.docx"}]}`)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			message := &discordgo.Message{ID: "1", ChannelID: "123", Author: &discordgo.User{ID: "2", Username: "alice"}, Attachments: []*discordgo.MessageAttachment{{Filename: "sample.docx", URL: "https://cdn.discordapp.com/attachments/1/2/sample.docx"}}}
			if mode == "caption" {
				message.Content = "Summarize"
			}
			if mode == "reply" {
				message = &discordgo.Message{ID: "2", ChannelID: "123", Author: message.Author, Content: "Summarize", ReferencedMessage: message}
			}
			if mode == "fetch reply" {
				message.Attachments = nil
				message.Content = "Summarize"
				message.MessageReference = &discordgo.MessageReference{MessageID: "parent"}
			}
			b.handleLLM(b.session, message)
			if len(model.requests) != 1 {
				t.Fatalf("requests=%d", len(model.requests))
			}
			docs := model.requests[0].Documents
			if len(docs) != 1 || !strings.Contains(docs[0].Text, "Revenue") || len(docs[0].Images) != 1 {
				t.Fatalf("missing text/images: %#v", docs)
			}
		})
	}
}

func TestDiscordDocumentDownloadLimits(t *testing.T) {
	b, err := New(config.Config{DiscordToken: "test", MaxFileSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("12345"))}, nil
	})}
	for _, tc := range []struct {
		url             string
		size, wantCalls int
	}{
		{"https://cdn.discordapp.com/attachments/1/2/file.docx", 0, 1},
		{"https://cdn.discordapp.com/attachments/1/2/file.docx", 5, 0},
		{"http://127.0.0.1/file.docx", 0, 0},
		{"https://evil.example/file.docx", 0, 0},
	} {
		calls = 0
		if _, err := b.downloadDocument(context.Background(), b.session, &discordgo.MessageAttachment{URL: tc.url, Size: tc.size}); err == nil {
			t.Error("unsafe/oversize download accepted")
		}
		if calls != tc.wantCalls {
			t.Errorf("calls=%d want=%d", calls, tc.wantCalls)
		}
	}
}
