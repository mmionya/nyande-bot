package resources

import (
	"strings"
	"testing"
)

func TestDiscordStringsExist(t *testing.T) {
	for _, key := range []string{
		"discord_help", "discord_ping", "discord_stats", "discord_downloading",
		"discord_sending", "discord_media_not_found", "discord_send_failed",
		"discord_download_too_large", "discord_download_not_found",
		"discord_download_timeout", "discord_download_failed", "discord_activity",
		"discord_llm_greeting", "discord_llm_failed", "discord_llm_disabled", "discord_reset_done", "discord_reset_empty",
		"discord_media_filename", "discord_gif_usage", "discord_gif_reply_failed",
		"discord_gif_no_video", "discord_gif_converting", "discord_gif_failed",
		"discord_gif_timeout", "discord_gif_too_large", "discord_gif_filename",
	} {
		t.Run(key, func(t *testing.T) {
			if Get(key) == "" {
				t.Fatal("empty string resource")
			}
		})
	}
}

func TestDiscordPrefixFormatting(t *testing.T) {
	for _, prefix := range []string{"!", "?", "ня!", "", "%s"} {
		t.Run(prefix, func(t *testing.T) {
			fields := map[string]any{"prefix": prefix}
			got := Format("discord_help", fields)
			for _, command := range []string{"help", "ping", "stats", "gif", "reset"} {
				if !strings.Contains(got, "`"+prefix+command+"`") {
					t.Fatalf("help does not contain prefixed command %q: %q", command, got)
				}
			}
			if strings.Contains(got, "{prefix}") {
				t.Fatalf("unresolved prefix: %q", got)
			}

			if got, want := Format("discord_activity", fields), prefix+"help | media links"; got != want {
				t.Fatalf("activity = %q, want %q", got, want)
			}
		})
	}
}
