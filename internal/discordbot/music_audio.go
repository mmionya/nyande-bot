package discordbot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// oggOpusReader reconstructs packets across page boundaries. FFmpeg is required
// to emit 20 ms Opus frames; neither an entire song nor decoded PCM is buffered.
type oggOpusReader struct {
	r        io.Reader
	lacing   []byte
	body     []byte
	partial  []byte
	serial   uint32
	sequence uint32
	started  bool
	headers  int
}

func (r *oggOpusReader) next() ([]byte, error) {
	for {
		if len(r.lacing) == 0 {
			var header [27]byte
			_, err := io.ReadFull(r.r, header[:])
			if err != nil {
				if err == io.EOF && len(r.partial) > 0 {
					return nil, io.ErrUnexpectedEOF
				}
				return nil, err
			}
			if string(header[:4]) != "OggS" || header[4] != 0 {
				return nil, errors.New("invalid Ogg page")
			}
			serial := binary.LittleEndian.Uint32(header[14:18])
			sequence := binary.LittleEndian.Uint32(header[18:22])
			if r.started && (serial != r.serial || sequence != r.sequence+1) {
				return nil, errors.New("discontinuous Ogg stream")
			}
			if (header[5]&1 != 0) != (len(r.partial) > 0) {
				return nil, errors.New("invalid Ogg packet continuation")
			}
			r.started = true
			r.serial = serial
			r.sequence = sequence
			r.lacing = make([]byte, int(header[26]))
			if _, err = io.ReadFull(r.r, r.lacing); err != nil {
				return nil, err
			}
			size := 0
			for _, n := range r.lacing {
				size += int(n)
			}
			r.body = make([]byte, size)
			if _, err = io.ReadFull(r.r, r.body); err != nil {
				return nil, err
			}
		}
		for len(r.lacing) > 0 {
			n := int(r.lacing[0])
			r.lacing = r.lacing[1:]
			if len(r.partial)+n > 64<<10 {
				return nil, errors.New("oversized Opus packet")
			}
			r.partial = append(r.partial, r.body[:n]...)
			r.body = r.body[n:]
			if n == 255 {
				continue
			}
			packet := r.partial
			r.partial = nil
			if r.headers == 0 {
				if len(packet) < 19 || !bytes.HasPrefix(packet, []byte("OpusHead")) || packet[9] != 2 {
					return nil, errors.New("expected stereo Opus header")
				}
				r.headers++
				continue
			}
			if r.headers == 1 {
				if !bytes.HasPrefix(packet, []byte("OpusTags")) {
					return nil, errors.New("missing Opus tags")
				}
				r.headers++
				continue
			}
			if len(packet) == 0 || len(packet) > 1275 {
				return nil, errors.New("invalid 20 ms Opus frame")
			}
			return packet, nil
		}
	}
}

type musicEncoder struct {
	frames chan []byte
	done   chan struct{}
	cancel context.CancelFunc
	err    error // read only after done/frames closes
}

func (p *musicEncoder) stop() { p.cancel(); <-p.done }
func musicFFmpegArgs(stream musicStream, offset int64, volume int) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "http,https,tcp,tls,crypto", "-rw_timeout", "15000000"}
	if offset > 0 {
		args = append(args, "-ss", strconv.FormatFloat(float64(offset)/1000, 'f', 3, 64))
	}
	// Only pass the source's required HTTP headers, without letting a header value
	// inject a second header. Cookies belong to yt-dlp's configured cookie jar.
	keys := []string{"User-Agent", "Referer", "Origin"}
	sort.Strings(keys)
	var headers strings.Builder
	for _, key := range keys {
		if v := stream.Headers[key]; v != "" && !strings.ContainsAny(v, "\r\n\x00") {
			headers.WriteString(key + ": " + v + "\r\n")
		}
	}
	if headers.Len() > 0 {
		args = append(args, "-headers", headers.String())
	}
	return append(args, "-i", stream.URL, "-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "2", "-ar", "48000", "-af", fmt.Sprintf("volume=%.2f", float64(volume)/100), "-c:a", "libopus", "-threads", "1", "-b:a", "64k", "-vbr", "on", "-frame_duration", "20", "-application", "audio", "-f", "opus", "-flush_packets", "1", "-page_duration", "20000", "pipe:1")
}
func startMusicEncoder(ctx context.Context, path string, stream musicStream, offset int64, volume int) (*musicEncoder, error) {
	return startMusicEncoderCommand(ctx, path, musicFFmpegArgs(stream, offset, volume))
}
func startMusicEncoderCommand(parent context.Context, path string, args []string) (*musicEncoder, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, path, args...)
	configureMusicProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = &boundedOutput{limit: 32 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = stdout.Close()
		return nil, err
	}
	p := &musicEncoder{frames: make(chan []byte, 2), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(p.done)
		defer close(p.frames)
		defer cancel()
		reader := oggOpusReader{r: stdout}
		for {
			frame, err := reader.next()
			if err != nil {
				if err != io.EOF {
					p.err = err
					cancel()
				}
				break
			}
			select {
			case p.frames <- frame:
			case <-ctx.Done():
				p.err = ctx.Err()
			}
			if ctx.Err() != nil {
				break
			}
		}
		waitErr := cmd.Wait()
		if p.err == nil {
			p.err = waitErr
		}
	}()
	return p, nil
}
