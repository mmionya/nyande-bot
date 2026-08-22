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
