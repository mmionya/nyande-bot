package downloader

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/music"
)

func TestMusicMetadataLimitCannotBeBypassedByCopy(t *testing.T) {
	var output musicMetadataOutput
	// Hide WriterTo to exercise io.Copy's ReaderFrom optimization, too.
	input := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", (4<<20)+1))}
	if _, err := io.Copy(&output, input); err == nil || output.buffer.Len() > 4<<20 {
		t.Fatal("metadata output exceeded its cap")
	}
}

func TestMusicSearchExtractsSupportedTracks(t *testing.T) {
	path := fakeYTDLPProcess(t, `printf '%s\n' "$@" > "$0.args"
printf '%s' '{"entries":[{"id":"abc","url":"abc","title":"Song","artist":"Artist","duration":123.5},{"url":"https://localhost/private","title":"Reject me"}]}'
`)
	d := New(config.Config{YTDownloadPath: path})
	tracks, err := d.SearchMusic(context.Background(), "yt: song & artist", 20)
	if err != nil || len(tracks) != 1 || tracks[0].URL != "https://www.youtube.com/watch?v=abc" || tracks[0].Duration != 123500*time.Millisecond {
		t.Fatalf("tracks=%+v err=%v", tracks, err)
	}
	args, _ := os.ReadFile(path + ".args")
	for _, arg := range []string{"--ignore-config\n", "--flat-playlist\n", "--playlist-end\n20\n", "--\nytsearch20:song & artist\n"} {
		if !strings.Contains(string(args), arg) {
			t.Fatalf("missing %q in args", arg)
		}
	}
}

func TestMusicSearchBoundedAndCancelled(t *testing.T) {
	for _, mode := range []string{"too much metadata", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			script := "head -c 4194305 /dev/zero"
			if mode == "cancelled" {
				script = "exec sleep 30"
			}
			d := New(config.Config{YTDownloadPath: fakeYTDLPProcess(t, script)})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			if _, err := d.SearchMusic(ctx, "song", 20); err == nil {
				t.Fatal("unbounded search succeeded")
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("search ignored cancellation")
			}
			// A failure must release the shared gate for the next request.
			check, done := context.WithTimeout(context.Background(), time.Second)
			defer done()
			release, err := music.Acquire(check)
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestMusicDownloadAudioBoundsAndCleanup(t *testing.T) {
	for _, mode := range []string{"ok", "oversized", "missing", "failure", "live", "long", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			script := `printf '%s\n' "$@" > "$0.args"
output=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = --output ]; then output="$2"; break; fi
 shift
done
printf '%s' "${output%/*}" > "$0.directory"
`
			switch mode {
			case "missing":
			case "failure":
				script += "printf 'private signed url secret-token' >&2\nexit 1\n"
			case "oversized":
				script += "printf '123456789012345678901234567890' > \"${output%.*}.mp3\"\n"
			default:
				script += "printf 'MP3 data' > \"${output%.*}.mp3\"\n"
			}
			path := fakeYTDLPProcess(t, script)
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			d := New(config.Config{YTDownloadPath: path, FFmpegPath: ffmpeg, MaxFileSize: 16})
			track := music.Track{URL: "https://soundcloud.com/artist/song", Duration: time.Minute}
			if mode == "live" {
				track.Live = true
			}
			if mode == "long" {
				track.Duration = 31 * time.Minute
			}
			if mode == "unsupported" {
				track.URL = "https://127.0.0.1/secret"
			}
			media, err := d.DownloadMusic(context.Background(), track)
			if mode == "ok" {
				if err != nil || media.Kind != "audio" || media.MIME != "audio/mpeg" || string(media.Data) != "MP3 data" {
					t.Fatalf("media=%+v err=%v", media, err)
				}
				args, _ := os.ReadFile(path + ".args")
				for _, arg := range []string{"--extract-audio\n", "--audio-format\nmp3\n", "--audio-quality\n128K\n", "--ffmpeg-location\n" + ffmpeg + "\n", "--max-filesize\n16\n", "--\nhttps://soundcloud.com/artist/song\n"} {
					if !strings.Contains(string(args), arg) {
						t.Fatalf("missing option %q", arg)
					}
				}
			} else if err == nil {
				t.Fatal("invalid download accepted")
			}
			if mode == "oversized" && !errors.Is(err, ErrMusicTooLarge) {
				t.Fatalf("size limit not reported: %v", err)
			}
			if (mode == "long" || mode == "live") && !errors.Is(err, ErrMusicTooLong) {
				t.Fatalf("duration not rejected: %v", err)
			}
			if mode == "failure" && strings.Contains(err.Error(), "secret-token") {
				t.Fatal("private diagnostics leaked")
			}
			if directory, readErr := os.ReadFile(path + ".directory"); readErr == nil {
				if _, statErr := os.Stat(filepath.Clean(string(directory))); !os.IsNotExist(statErr) {
					t.Fatal("temporary audio was not removed")
				}
			} else if mode != "live" && mode != "long" && mode != "unsupported" {
				t.Fatal(readErr)
			}
		})
	}
}
