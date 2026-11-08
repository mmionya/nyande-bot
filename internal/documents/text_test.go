package documents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextAttachmentsPreserveCodeAndRejectBinary(t *testing.T) {
	const content = "  # Привет\r\n    print('**code**')\n\n"
	for _, name := range []string{"code.py", "report.csv", "notes.txt", "data.json", "readme.md", "script.go"} {
		for _, images := range []int{0, 6} {
			doc, err := Read(context.Background(), name, "application/octet-stream", []byte("\ufeff"+content), images)
			if err != nil || doc.Text != content || doc.Warning != "" || doc.Truncated || len(doc.Images) != 0 {
				t.Fatalf("%s: %+v, %v", name, doc, err)
			}
		}
	}
	for _, data := range [][]byte{{0xff, 0xfe}, []byte("a\x00b"), []byte("a\x01b")} {
		if _, err := Read(context.Background(), "binary.txt", "text/plain", data, 6); err == nil {
			t.Fatalf("binary input accepted: %v", data)
		}
	}
	if _, err := Extract(context.Background(), "empty.txt", "", []byte(" \n\t")); !errors.Is(err, ErrNoText) {
		t.Fatalf("empty text: %v", err)
	}
}

func TestTextAttachmentLimitsAndCancellation(t *testing.T) {
	doc, err := Read(context.Background(), "large.jsonl", "", []byte(strings.Repeat("猫", MaxTextRunes+1)), 6)
	if err != nil || !doc.Truncated || len([]rune(doc.Text)) != MaxTextRunes || !utf8.ValidString(doc.Text) {
		t.Fatalf("rune limit: length=%d truncated=%v err=%v", len([]rune(doc.Text)), doc.Truncated, err)
	}
	if _, err := Read(context.Background(), "large.txt", "", make([]byte, MaxFileSize+1), 0); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, "data.csv", "text/csv", []byte("a,b"), 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
