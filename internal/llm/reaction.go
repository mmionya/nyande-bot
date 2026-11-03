package llm

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

// Standard Telegram reactions also usable as Unicode reactions in Discord.
// Keep fully qualified emoji here; the Telegram adapter removes variation selectors.
var reactionEmojis = strings.Fields("👍 👎 ❤️ 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊️ 🤡 🥱 🥴 😍 🐳 ❤️‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍️ 🤗 🫡 🎅 🎄 ☃️ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂️ 🤷 🤷‍♀️ 😡")

// ReactionTool is scoped to the triggering message and its direct reply parent.
// apply resolves these symbolic targets; the model cannot supply message/chat IDs.
func ReactionTool(hasReply bool, apply func(context.Context, string, string) error) Tool {
	targets := []string{"current"}
	if hasReply {
		targets = append(targets, "reply")
	}
	var mu sync.Mutex
	attempts := make(map[string]string)
	return Tool{
		Name:        "react_to_message",
		Description: resources.Get("tool.reaction.description"),
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"emoji":  map[string]any{"type": "string", "enum": append([]string(nil), reactionEmojis...)},
				"target": map[string]any{"type": "string", "enum": targets, "description": resources.Get("tool.reaction.target")},
			},
			"required": []string{"emoji"},
		},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			for key := range args {
				if key != "emoji" && key != "target" {
					return resources.Get("tool.reaction.invalid"), nil
				}
			}
			target := strings.TrimSpace(args["target"])
			if target == "" {
				target = "current"
			}
			if target != "current" && (target != "reply" || !hasReply) {
				return resources.Get("tool.reaction.invalid"), nil
			}
			emoji := normalizeReaction(args["emoji"])
			if emoji == "" {
				return resources.Get("tool.reaction.invalid"), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if previous, ok := attempts[target]; ok {
				return resources.Format("tool.reaction.already_attempted", map[string]any{"outcome": previous}), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			reactionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			outcome := resources.Get("tool.reaction.failed")
			if err := apply(reactionCtx, target, emoji); err == nil {
				outcome = resources.Format("tool.reaction.done", map[string]any{"emoji": emoji, "target": target})
			}
			// Include failures: a lost response must not trigger repeated attempts.
			attempts[target] = outcome
			return outcome, nil
		},
	}
}

func normalizeReaction(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\ufe0f", "")
	for _, emoji := range reactionEmojis {
		if value == strings.ReplaceAll(emoji, "\ufe0f", "") {
			return emoji
		}
	}
	return ""
}
