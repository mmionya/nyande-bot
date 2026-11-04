package music

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type searchTransport func(*http.Request) (*http.Response, error)

func (f searchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSharedSearchSources(t *testing.T) {
	for query, want := range map[string]string{"yt: song": "ytsearch5:song", "ytm: song": "https://music.youtube.com/search?q=song#songs", "sc: song": "scsearch5:song", "https://youtu.be/song": "https://youtu.be/song"} {
		identifier, err := Identifier(query)
		if err != nil {
			t.Fatal(err)
		}
		run := func(_ context.Context, args ...string) ([]byte, error) {
			if args[len(args)-1] != want {
				t.Fatalf("target %q want %q", args[len(args)-1], want)
			}
			return []byte(`{"title":"song","webpage_url":"https://youtu.be/song"}`), nil
		}
		tracks, err := Search(context.Background(), identifier, 5, run, http.DefaultClient)
		if err != nil || len(tracks) != 1 {
			t.Fatalf("query=%q tracks=%v err=%v", query, tracks, err)
		}
	}
}

func TestSharedBandcampSearch(t *testing.T) {
	client := &http.Client{Transport: searchTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "bandcamp.com" || r.URL.Query().Get("q") != "a & b" {
			t.Fatalf("unexpected Bandcamp target %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<li class="searchresult"><div class="heading"><a href="https://artist.bandcamp.com/track/song">Song</a></div><div class="subhead">by Artist</div></li>`))}, nil
	})}
	tracks, err := Search(context.Background(), "bcsearch:a & b", 5, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("Bandcamp search launched yt-dlp")
		return nil, nil
	}, client)
	if err != nil || len(tracks) != 1 || tracks[0].Author != "Artist" {
		t.Fatalf("tracks=%v err=%v", tracks, err)
	}
}

func TestSharedMusicGateHonorsDeadline(t *testing.T) {
	release, err := Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked extraction ignored deadline: %v", err)
	}
}
