package discordbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/gorilla/websocket"
	"github.com/mmionya/nyande-bot/internal/config"
)

func TestMusicLavalinkHandshakeAndPayload(t *testing.T) {
	updates := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-password" {
			t.Error("missing Lavalink authentication")
		}
		switch {
		case r.URL.Path == "/v4/websocket":
			if r.Header.Get("User-Id") != "99" {
				t.Error("wrong bot ID in voice handshake")
			}
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			if err := conn.WriteJSON(map[string]any{"op": "ready", "resumed": false, "sessionId": "test-session"}); err != nil {
				t.Error(err)
				return
			}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case r.URL.Path == "/v4/loadtracks":
			if r.URL.Query().Get("identifier") != "ytsearch:a song" {
				t.Error("wrong search query")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"loadType":"search","data":[{"encoded":"track-data","info":{"title":"a song","identifier":"a"}}]}`))
		case r.URL.Path == "/v4/sessions/test-session/players/1" && r.Method == http.MethodPatch:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			updates <- payload
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"guildId":"1","volume":100,"paused":false,"voice":{},"state":{},"filters":{}}`))
		case r.URL.Path == "/v4/sessions/test-session/players/1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Lavalink request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	session, _ := discordgo.New("Bot test")
	session.State.User = &discordgo.User{ID: "99"}
	b := &Bot{cfg: config.Config{MusicBackend: "lavalink", LavalinkAddress: strings.TrimPrefix(server.URL, "http://"), LavalinkPassword: "test-password"}}
	service, err := b.getMusic(session)
	if err != nil {
		t.Fatal(err)
	}
	defer service.backend.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tracks, err := service.backend.load(ctx, "ytsearch:a song")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("load failed: %v", err)
	}
	if err := service.backend.update(ctx, "1", disgolink.WithTrack(tracks[0]), disgolink.WithPaused(false)); err != nil {
		t.Fatal(err)
	}
	if err := service.backend.update(ctx, "1", disgolink.WithVoice(lavalink.VoiceState{ChannelID: 10, SessionID: "discord-session", Token: "voice-token", Endpoint: "voice.example"})); err != nil {
		t.Fatal(err)
	}
	first := <-updates
	if first["track"].(map[string]any)["encoded"] != "track-data" {
		t.Fatalf("wrong track payload: %#v", first)
	}
	voice := (<-updates)["voice"].(map[string]any)
	if voice["channelId"] != "10" || voice["sessionId"] != "discord-session" || voice["token"] != "voice-token" {
		t.Fatalf("wrong DAVE voice payload: %#v", voice)
	}
	if err := service.backend.destroy(ctx, "1"); err != nil {
		t.Fatal(err)
	}
}
