package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvList(t *testing.T) {
	t.Setenv("TEST_TRIGGER_WORDS", " Мяу, нянде;МЯУ\nэй бот ")
	want := []string{"мяу", "нянде", "эй бот"}
	if got := envList("TEST_TRIGGER_WORDS", []string{"fallback"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("envList() = %#v, want %#v", got, want)
	}
}

func TestEnvListFallback(t *testing.T) {
	t.Setenv("TEST_TRIGGER_WORDS", "")
	want := []string{"мяу"}
	if got := envList("TEST_TRIGGER_WORDS", want); !reflect.DeepEqual(got, want) {
		t.Fatalf("envList() = %#v, want %#v", got, want)
	}
}

func TestLoadAcceptsDiscordTokenWithoutTelegramToken(t *testing.T) {
	t.Setenv("BOT_TOKEN", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "discord-token")
	t.Setenv("DISCORD_COMMAND_PREFIX", "?")
	t.Setenv("LLM_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BotToken != "" || cfg.DiscordToken != "discord-token" || cfg.DiscordPrefix != "?" {
		t.Fatalf("unexpected platform configuration: %#v", cfg)
	}
}

func TestLoadRequiresAtLeastOneBotToken(t *testing.T) {
	t.Setenv("BOT_TOKEN", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("LLM_ENABLED", "false")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BOT_TOKEN or DISCORD_BOT_TOKEN") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestDirectMusicDefaults(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "test")
	t.Setenv("LLM_ENABLED", "false")
	t.Setenv("DISCORD_MUSIC_BACKEND", "")
	t.Setenv("MUSIC_MAX_PLAYERS", "")
	t.Setenv("FFMPEG_PATH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MusicBackend != "direct" || cfg.MusicMaxPlayers != 1 || cfg.FFmpegPath != "ffmpeg" {
		t.Fatal("wrong direct music defaults")
	}
	t.Setenv("MUSIC_MAX_PLAYERS", "3")
	t.Setenv("FFMPEG_PATH", "/opt/media/ffmpeg")
	cfg, err = Load()
	if err != nil || cfg.MusicMaxPlayers != 3 || cfg.FFmpegPath != "/opt/media/ffmpeg" {
		t.Fatal("music configuration override failed", err)
	}
}

func TestBanConfiguration(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test")
	t.Setenv("LLM_ENABLED", "false")
	t.Setenv("BOT_OWNER_IDS", "1, 2;1")
	t.Setenv("DISCORD_OWNER_IDS", "18446744073709551615")
	t.Setenv("BANNED_USERS_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.BotOwnerIDs, []string{"1", "2"}) || !reflect.DeepEqual(cfg.DiscordOwnerIDs, []string{"18446744073709551615"}) || cfg.BannedUsersFile != ".nyande-banned-users.json" {
		t.Fatal("unexpected ban configuration")
	}
	t.Setenv("BANNED_USERS_FILE", "/tmp/custom-bans.json")
	cfg, err = Load()
	if err != nil || cfg.BannedUsersFile != "/tmp/custom-bans.json" {
		t.Fatalf("custom ban path not loaded: %v", err)
	}
	for _, name := range []string{"BOT_OWNER_IDS", "DISCORD_OWNER_IDS"} {
		for _, invalid := range []string{"1,invalid", "0", "01", "+1", "-1", "18446744073709551616"} {
			t.Run(name+"/"+invalid, func(t *testing.T) {
				t.Setenv(name, invalid)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("invalid owner configuration accepted: %v", err)
				}
			})
		}
	}
}
