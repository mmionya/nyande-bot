package webpage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	rawHTML := `
<!DOCTYPE html>
<html>
<head>
    <title>Тестовая статья</title>
    <style>body { font-size: 16px; }</style>
    <script>alert("hello");</script>
</head>
<body>
    <header><nav><a href="/">Home</a></nav></header>
    <h1>Заголовок статьи</h1>
    <p>Это первый параграф с <b>жирным</b> текстом &amp; спецсимволами.</p>
    <p>Второй параграф текста.</p>
    <footer>Copyright 2026</footer>
</body>
</html>
`
	text := ExtractText(rawHTML)
	if strings.Contains(text, "alert") || strings.Contains(text, "font-size") {
		t.Fatalf("style/script was not stripped: %s", text)
	}
	if strings.Contains(text, "Home") || strings.Contains(text, "Copyright") {
		t.Fatalf("nav/footer was not stripped: %s", text)
	}
	if !strings.Contains(text, "Заголовок статьи") || !strings.Contains(text, "первый параграф") || !strings.Contains(text, "& спецсимволами") {
		t.Fatalf("content was not extracted properly: %s", text)
	}
}

func TestFetchServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>My Title</title></head><body><h1>Hello World</h1><p>Test content here.</p></body></html>`))
	}))
	defer server.Close()

	page, err := Fetch(context.Background(), server.URL, 1000)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if page.Title != "My Title" {
		t.Fatalf("expected title 'My Title', got %q", page.Title)
	}
	if !strings.Contains(page.Content, "Hello World") || !strings.Contains(page.Content, "Test content here.") {
		t.Fatalf("expected content, got %q", page.Content)
	}
}
