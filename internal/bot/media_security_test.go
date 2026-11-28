package bot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalMediaRejectsNetworkPlaylists(t *testing.T) {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is required for the real decoder security test")
		}
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected playlist request", http.StatusForbidden)
	}))
	defer server.Close()
	playlist := []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n" + server.URL + "/private.ts\n#EXT-X-ENDLIST\n")
	b := concurrentTestBot()
	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"audio", func(ctx context.Context) error {
			_, err := b.convertMediaToAudio(ctx, playlist, "video.mp4")
			return err
		}},
		{"voice", func(ctx context.Context) error {
			_, err := convertAudioToVoice(ctx, playlist, "voice.ogg", "")
			return err
		}},
		{"round", func(ctx context.Context) error {
			_, err := convertVideoToRound(ctx, playlist, "video.mp4", "")
			return err
		}},
		{"gif", func(ctx context.Context) error {
			_, err := convertVideoToGIF(ctx, playlist, "video.mp4", "")
			return err
		}},
		{"frames", func(ctx context.Context) error {
			_, err := b.extractVideoFrames(ctx, playlist, "video.mp4", 1)
			return err
		}},
		{"transcription", func(ctx context.Context) error {
			_, err := b.transcribe(ctx, cachedAttachment{Name: "voice.ogg", Data: playlist}, false)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := test.run(ctx); err == nil {
				t.Fatal("playlist accepted as local media")
			}
			if requests.Load() != 0 {
				t.Fatal("decoder followed a URL from untrusted media")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = inspectMedia(ctx, cachedAttachment{Kind: "video", Name: "video.mp4", Data: playlist}, "")
	if requests.Load() != 0 {
		t.Fatal("inspection followed a playlist URL")
	}
}
