package resources

import "testing"

func TestDiscordStringsExist(t *testing.T) {
	for _, key := range []string{
		"discord_help", "discord_ping", "discord_stats", "discord_downloading",
		"discord_sending", "discord_media_not_found", "discord_send_failed",
		"discord_download_too_large", "discord_download_not_found",
		"discord_download_timeout", "discord_download_failed", "discord_activity",
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
			want := "Пришли ссылку — я скачаю фото, видео или карусель из TikTok, Instagram, X/Twitter, Xiaohongshu, Pinterest, YouTube или Reddit. Также поддерживаются прямые ссылки на медиа.\n\nКоманды: `" + prefix + "help`, `" + prefix + "ping`, `" + prefix + "stats`, `" + prefix + "gif`.\n`" + prefix + "gif` — сделать GIF из прикреплённого видео, ссылки или видео в сообщении, на которое ты отвечаешь."
			if got := Format("discord_help", fields); got != want {
				t.Fatalf("help = %q, want %q", got, want)
			}
			if got, want := Format("discord_activity", fields), prefix+"help | media links"; got != want {
				t.Fatalf("activity = %q, want %q", got, want)
			}
		})
	}
}
