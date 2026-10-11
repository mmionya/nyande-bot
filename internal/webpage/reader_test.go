package webpage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	transport := defaultClient.Transport
	defaultClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: r,
			Body: io.NopCloser(strings.NewReader(`<html><head><title>My Title</title></head><body><h1>Hello World</h1><p>Test content here.</p></body></html>`))}, nil
	})
	t.Cleanup(func() { defaultClient.Transport = transport })

	page, err := Fetch(context.Background(), "https://example.com/article", 1000)
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

func TestFetchRejectsPrivateTargets(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "private service")
	}))
	defer server.Close()
	transport := defaultClient.Transport
	protected := transport
	if protected == nil {
		protected = http.DefaultTransport
	}
	defaultClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "example.com" {
			return &http.Response{StatusCode: http.StatusFound, Request: r,
				Header: http.Header{"Location": {server.URL}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		// Redirect targets still pass through the real production transport.
		return protected.RoundTrip(r)
	})
	t.Cleanup(func() { defaultClient.Transport = transport })
	for _, target := range []string{
		server.URL,
		strings.Replace(server.URL, "127.0.0.1", "localhost", 1),
		"https://example.com/redirect",
	} {
		if _, err := Fetch(context.Background(), target, 1000); err == nil {
			t.Errorf("Fetch(%q) reached a private service", target)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("private service received %d requests; expected none", hits.Load())
	}
}

func TestPublicTransportAddressGuard(t *testing.T) {
	if defaultClient.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("an environment proxy could bypass public address checks")
	}
	for _, address := range []string{
		"8.8.8.8:443", "[2606:4700:4700::1111]:80",
	} {
		if err := checkPublicAddress("tcp", address, nil); err != nil {
			t.Errorf("public endpoint %s rejected: %v", address, err)
		}
	}
	for _, address := range []string{
		"127.0.0.1:80", "10.1.2.3:80", "172.16.1.2:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.100.100.200:80", "0.0.0.0:80", "224.0.0.1:80",
		"[::1]:80", "[::]:80", "[fe80::1]:80", "[fd00::1]:80", "[ff02::1]:80",
		"[::ffff:127.0.0.1]:80", "localhost:80", "invalid",
	} {
		if err := checkPublicAddress("tcp", address, nil); err == nil {
			t.Errorf("non-public endpoint %s allowed", address)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
