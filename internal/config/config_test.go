package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDotEnvUsesLastValueAndPreservesProcessEnvironment(t *testing.T) {
	const fileKey = "NYANDE_TEST_FILE_VALUE"
	const processKey = "NYANDE_TEST_PROCESS_VALUE"
	t.Setenv(fileKey, "")
	if err := os.Unsetenv(fileKey); err != nil {
		t.Fatal(err)
	}
	t.Setenv(processKey, "process")
	path := filepath.Join(t.TempDir(), ".env")
	data := fileKey + "=first\nexport " + fileKey + "='last'\n" + processKey + "=file\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(fileKey) != "last" || os.Getenv(processKey) != "process" {
		t.Fatal(".env precedence differs from service EnvironmentFile precedence")
	}
}

func TestLoadRejectsUnreadableDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BOT_TOKEN", "test")
	t.Setenv("LLM_ENABLED", "false")
	if err := os.Mkdir(".env", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "load .env") {
		t.Fatalf("must report .env read errors instead of using defaults: %v", err)
	}
}

func TestStorageEncryptionKeyConfiguration(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test")
	t.Setenv("LLM_ENABLED", "false")
	path := filepath.Join(t.TempDir(), "storage.key")
	t.Setenv("DATA_ENCRYPTION_KEY_FILE", path)
	if _, err := Load(); err == nil {
		t.Fatal("missing encryption key must prevent startup")
	}
	key := bytes.Repeat([]byte{71}, 32)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || !bytes.Equal(cfg.StorageEncryptionKey, key) {
		t.Fatalf("encryption key not loaded: %v", err)
	}
	if strings.Contains(cfg.String(), string(key)) {
		t.Fatal("configuration summary exposes the encryption key")
	}
}

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

func TestVideoCompressionConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BOT_TOKEN", "test")
	t.Setenv("LLM_ENABLED", "false")
	for _, name := range []string{
		"BOT_OWNER_IDS", "DISCORD_OWNER_IDS", "DATA_ENCRYPTION_KEY_FILE",
		"VIDEO_COMPRESSION_ENABLED", "VIDEO_MAX_SOURCE_SIZE",
		"VIDEO_COMPRESSION_TIMEOUT_SECONDS", "VIDEO_MAX_DURATION_SECONDS", "FFPROBE_PATH",
	} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VideoCompressionEnabled || cfg.VideoMaxSourceSize != 300*1024*1024 ||
		cfg.VideoCompressionTimeout != 5*time.Minute || cfg.VideoMaxDuration != 10*time.Minute ||
		cfg.FFprobePath != "ffprobe" {
		t.Fatal("unexpected video compression defaults")
	}

	t.Setenv("VIDEO_COMPRESSION_ENABLED", "false")
	t.Setenv("VIDEO_MAX_SOURCE_SIZE", "104857600")
	t.Setenv("VIDEO_COMPRESSION_TIMEOUT_SECONDS", "90.5")
	t.Setenv("VIDEO_MAX_DURATION_SECONDS", "120")
	t.Setenv("FFPROBE_PATH", "/opt/media/ffprobe")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VideoCompressionEnabled || cfg.VideoMaxSourceSize != 100*1024*1024 ||
		cfg.VideoCompressionTimeout != 90500*time.Millisecond || cfg.VideoMaxDuration != 2*time.Minute ||
		cfg.FFprobePath != "/opt/media/ffprobe" {
		t.Fatal("video compression overrides not applied")
	}

	for _, invalid := range []string{"0", "1023", "2147483649", "-1", "invalid"} {
		t.Run("source_size/"+invalid, func(t *testing.T) {
			t.Setenv("VIDEO_MAX_SOURCE_SIZE", invalid)
			cfg, err := Load()
			if err != nil || cfg.VideoMaxSourceSize != 300*1024*1024 {
				t.Fatalf("invalid source limit must use the default: %v", err)
			}
		})
	}
	for _, name := range []string{"VIDEO_COMPRESSION_TIMEOUT_SECONDS", "VIDEO_MAX_DURATION_SECONDS"} {
		for _, invalid := range []string{"0", "-1", "NaN", "+Inf", "-Inf", "1e30", "1e-12", "invalid"} {
			t.Run(name+"/"+invalid, func(t *testing.T) {
				t.Setenv(name, invalid)
				cfg, err := Load()
				if err != nil {
					t.Fatal(err)
				}
				if name == "VIDEO_COMPRESSION_TIMEOUT_SECONDS" && cfg.VideoCompressionTimeout != 5*time.Minute ||
					name == "VIDEO_MAX_DURATION_SECONDS" && cfg.VideoMaxDuration != 10*time.Minute {
					t.Fatal("invalid duration must preserve a positive default safety limit")
				}
			})
		}
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
