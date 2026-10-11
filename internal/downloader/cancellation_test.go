package downloader

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestDownloadCancelsPendingMediaDNS(t *testing.T) {
	started := make(chan struct{}, 1)
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(func() { net.DefaultResolver = previous })
	d := New(config.Config{MaxFileSize: 1024, MaxMediaItems: 1, RetryAttempts: 3})
	d.client.Transport = mediaRedirectTransport(func(*http.Request) (*http.Response, error) {
		t.Error("cancelled DNS lookup must not reach the HTTP transport")
		return nil, errors.New("unexpected HTTP request")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx, "https://cancel-media.example/video.mp4")
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("media DNS lookup did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download kept waiting for DNS after cancellation")
	}
}
