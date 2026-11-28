package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShutdownDrainsWorkersBeforeClosing(t *testing.T) {
	for _, stop := range []string{"cancel", "close"} {
		t.Run(stop, func(t *testing.T) {
			b := concurrentTestBot()
			ctx, cancel := context.WithCancel(context.Background())
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, startOnce sync.Once
			runDone, closeDone := make(chan error, 1), make(chan error, 1)
			previous := http.DefaultTransport
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(release) })
				_ = b.Close()
				http.DefaultTransport = previous
			})
			updates := 0
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":true}`
				switch filepath.Base(r.URL.Path) {
				case "getMe":
					body = `{"ok":true,"result":{"id":99,"is_bot":true,"username":"test_bot"}}`
				case "getUpdates":
					updates++
					if updates > 1 {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					// Fill all workers and block the producer on its semaphore.
					var batch []string
					for id := 1; id <= 30; id++ {
						batch = append(batch, fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"chat":{"id":123,"type":"private"},"text":"/ping"}}`, id, id))
					}
					body = `{"ok":true,"result":[` + strings.Join(batch, ",") + `]}`
				case "sendMessage":
					startOnce.Do(func() { close(started) })
					<-release // Simulate an already-started operation still cleaning up.
					body = `{"ok":true,"result":{"message_id":100}}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			go func() { runDone <- b.Run(ctx) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not start")
			}
			if stop == "cancel" {
				cancel()
			}
			go func() { closeDone <- b.Close() }()
			select {
			case <-runDone:
				t.Fatal("Run returned before its worker finished")
			case <-closeDone:
				t.Fatal("Close returned before its worker finished")
			case <-time.After(50 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-runDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run failed to drain cancelled work")
			}
			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close failed to finish")
			}
			if err := b.Run(context.Background()); err == nil {
				t.Fatal("closed bot restarted")
			}
		})
	}
}
