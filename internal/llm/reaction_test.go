package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReactionValidationAndTargets(t *testing.T) {
	for _, hasReply := range []bool{false, true} {
		calls := 0
		tool := ReactionTool(hasReply, func(ctx context.Context, target, emoji string) error {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("reaction has no deadline")
			}
			if emoji != "❤️" {
				t.Errorf("emoji not normalized: %q", emoji)
			}
			return nil
		})
		invalid := []map[string]string{
			nil, {"emoji": "not an emoji"}, {"emoji": "👍🔥"}, {"emoji": "<:cat:123>"},
			{"emoji": "👍", "target": "123"}, {"emoji": "👍", "message_id": "123"},
			{"emoji": "👍", "chat_id": "456"},
		}
		if !hasReply {
			invalid = append(invalid, map[string]string{"emoji": "👍", "target": "reply"})
		}
		for _, args := range invalid {
			result, err := tool.Execute(context.Background(), args)
			if err != nil || calls != 0 || !strings.Contains(result, "not sent") {
				t.Fatalf("invalid arguments caused reaction: %v %q %v", args, result, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := tool.Execute(ctx, map[string]string{"emoji": "❤"}); !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("canceled call: %v", err)
		}
		first, err := tool.Execute(context.Background(), map[string]string{"emoji": " ❤ "})
		if err != nil || calls != 1 || !strings.Contains(first, "successfully set on target=current") {
			t.Fatalf("current: %q %v calls=%d", first, err, calls)
		}
		second, err := tool.Execute(context.Background(), map[string]string{"emoji": "🔥", "target": "current"})
		if err != nil || calls != 1 || !strings.Contains(second, first) {
			t.Fatalf("repeated reaction: %q %v calls=%d", second, err, calls)
		}
		if hasReply {
			result, err := tool.Execute(context.Background(), map[string]string{"emoji": "❤️", "target": "reply"})
			if err != nil || calls != 2 || !strings.Contains(result, "successfully set on target=reply") {
				t.Fatalf("reply: %q %v calls=%d", result, err, calls)
			}
		}
	}
}

func TestFailedReactionIsNotRetriedOrClaimed(t *testing.T) {
	calls := 0
	tool := ReactionTool(false, func(context.Context, string, string) error { calls++; return errors.New("secret token") })
	for range 2 {
		result, err := tool.Execute(context.Background(), map[string]string{"emoji": "👍"})
		if err != nil || !strings.Contains(result, "not confirmed") || strings.Contains(result, "secret token") || strings.Contains(result, "successfully set") {
			t.Fatalf("failure: %q %v", result, err)
		}
	}
	if calls != 1 {
		t.Fatalf("failed reaction retried %d times", calls)
	}
}
