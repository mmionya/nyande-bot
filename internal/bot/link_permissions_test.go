package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func permissionTestBot(t *testing.T) (*Bot, *[]int) {
	t.Helper()
	settings, err := loadLinkDeletionSettings(filepath.Join(t.TempDir(), "moderation.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []int64{-1, -2} {
		if err := settings.EnableSilent(chat); err != nil {
			t.Fatal(err)
		}
	}
	allowed, err := loadAllowlist(filepath.Join(t.TempDir(), "allowed.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{linkConfig: settings, allowlist: allowed, telegram: telegram.New("test"), state: NewState(), identity: telegram.User{ID: 99, Username: "nyande_test_bot"}, adminCache: map[adminKey]adminEntry{}}
	for _, chat := range []int64{-1, -2} {
		for _, user := range []int64{1, 2, 3} {
			b.adminCache[adminKey{ChatID: chat, UserID: user}] = adminEntry{admin: user == 1, expires: time.Now().Add(time.Hour)}
		}
	}
	deleted := []int{}
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/deleteMessage") {
			t.Errorf("silent mode made unexpected Telegram request: %s", r.URL.Path)
		}
		var body struct {
			MessageID int `json:"message_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		deleted = append(deleted, body.MessageID)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return b, &deleted
}
func permissionMessage(id int, from int64, text string) *telegram.Message {
	return &telegram.Message{MessageID: id, From: &telegram.User{ID: from}, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, Text: text}
}
func permissionUpdate(t *testing.T, b *Bot, m *telegram.Message, edited bool) {
	t.Helper()
	update := telegram.Update{Message: m}
	if edited {
		update = telegram.Update{EditedMessage: m}
	}
	if err := b.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
}

func TestDiscussionChannelAutomaticPosts(t *testing.T) {
	b, deleted := permissionTestBot(t)
	var post telegram.Message
	if err := json.Unmarshal([]byte(`{"message_id":10,"chat":{"id":-1,"type":"supergroup"},"sender_chat":{"id":-100,"type":"channel"},"from":{"id":777000,"is_bot":true},"is_automatic_forward":true,"text":"https://example.com/post"}`), &post); err != nil {
		t.Fatal(err)
	}
	if !post.IsAutomaticForward {
		t.Fatal("automatic-forward flag was not decoded")
	}
	permissionUpdate(t, b, &post, false)
	permissionUpdate(t, b, &post, true)
	post.Text = ""
	post.Caption = "photo with link"
	post.CaptionEntities = []telegram.MessageEntity{{Type: "text_link", URL: "https://example.com/photo", Length: 5}}
	permissionUpdate(t, b, &post, false)
	permissionUpdate(t, b, &post, true)
	if len(*deleted) != 0 {
		t.Fatal("deleted an automatic channel post")
	}
	manual := permissionMessage(11, 2, "https://example.com/manual")
	manual.ForwardOrigin = &telegram.MessageOrigin{Type: "channel", Chat: &telegram.Chat{ID: -100, Type: "channel"}}
	permissionUpdate(t, b, manual, false)
	comment := permissionMessage(12, 2, "https://example.com/comment")
	comment.ReplyToMessage = &post
	permissionUpdate(t, b, comment, false)
	impersonation := permissionMessage(13, 1, "https://example.com/channel")
	impersonation.SenderChat = &telegram.Chat{ID: -200, Type: "channel"}
	permissionUpdate(t, b, impersonation, false)
	if len(*deleted) != 3 {
		t.Fatalf("manual forwards, replies and other channels must remain moderated: %v", *deleted)
	}
}
func TestPermanentLinkPermissionGrantRevokeAndScope(t *testing.T) {
	b, deleted := permissionTestBot(t)
	target := permissionMessage(1, 2, "Можно ссылку?")
	grant := permissionMessage(2, 1, "/permitlinks@nyande_test_bot")
	grant.ReplyToMessage = target
	permissionUpdate(t, b, grant, false)
	if !b.linkConfig.UserPermitted(-1, 2) || b.linkConfig.UserPermitted(-2, 2) {
		t.Fatal("incorrect grant scope")
	}
	for _, edited := range []bool{false, true} {
		permissionUpdate(t, b, permissionMessage(3, 2, "https://example.com/a https://youtube.com/watch?v=x"), edited)
		caption := permissionMessage(4, 2, "")
		caption.Caption = "click"
		caption.CaptionEntities = []telegram.MessageEntity{{Type: "text_link", URL: "https://example.com", Length: 5}}
		permissionUpdate(t, b, caption, edited)
	}
	if len(*deleted) != 0 {
		t.Fatal("permitted links were deleted")
	}
	restarted, err := loadLinkDeletionSettings(b.linkConfig.path)
	if err != nil {
		t.Fatal(err)
	}
	b.linkConfig = restarted
	permissionUpdate(t, b, permissionMessage(5, 2, "https://example.com/persisted"), false)
	unrelated := permissionMessage(6, 2, "https://example.com/other-chat")
	unrelated.Chat.ID = -2
	permissionUpdate(t, b, unrelated, false)
	permissionUpdate(t, b, permissionMessage(7, 3, "https://example.com/other-user"), false)
	if len(*deleted) != 2 {
		t.Fatal("permission leaked", *deleted)
	}
	revoke := permissionMessage(8, 1, "/revokelinks")
	revoke.ReplyToMessage = target
	permissionUpdate(t, b, revoke, false)
	restarted, err = loadLinkDeletionSettings(b.linkConfig.path)
	if err != nil {
		t.Fatal(err)
	}
	b.linkConfig = restarted
	if b.linkConfig.UserPermitted(-1, 2) {
		t.Fatal("revoke did not persist")
	}
	permissionUpdate(t, b, permissionMessage(9, 2, "https://example.com/after-revoke"), false)
	permissionUpdate(t, b, permissionMessage(10, 2, "https://example.com/edited-after-revoke"), true)
	if len(*deleted) != 4 {
		t.Fatal("revoked links were not deleted", *deleted)
	}
	if !b.linkConfig.Silent(-1) || !b.linkConfig.Enabled(-1) {
		t.Fatal("permission command disabled moderation")
	}
}
func TestLinkPermissionAuthorizationAndCommandBypasses(t *testing.T) {
	b, deleted := permissionTestBot(t)
	permissionUpdate(t, b, permissionMessage(1, 2, "/permitlinks 2"), false)
	permissionUpdate(t, b, permissionMessage(2, 2, "/permitlinks https://example.com"), false)
	permissionUpdate(t, b, permissionMessage(3, 1, "/permitlinks@another_bot 2"), false)
	permissionUpdate(t, b, permissionMessage(4, 1, "/permitlinks 2"), true)
	forwarded := permissionMessage(5, 1, "/permitlinks 2")
	forwarded.ForwardOrigin = &telegram.MessageOrigin{Type: "user", SenderUser: &telegram.User{ID: 1}}
	permissionUpdate(t, b, forwarded, false)
	otherChannel := permissionMessage(6, 1, "/permitlinks 2")
	otherChannel.SenderChat = &telegram.Chat{ID: -100, Type: "channel"}
	permissionUpdate(t, b, otherChannel, false)
	if b.linkConfig.UserPermitted(-1, 2) {
		t.Fatal("unauthorized/forwarded/edited command granted permission")
	}
	if len(*deleted) != 1 || (*deleted)[0] != 2 {
		t.Fatal("command text bypassed link deletion", *deleted)
	}
	// An anonymous administrator sends on behalf of this group, not a channel.
	anonymous := permissionMessage(7, 777000, "/permitlinks 2")
	anonymous.SenderChat = &telegram.Chat{ID: -1, Type: "supergroup"}
	permissionUpdate(t, b, anonymous, false)
	permissionUpdate(t, b, permissionMessage(8, 2, "/permitlinks 3"), false)
	if !b.linkConfig.UserPermitted(-1, 2) || b.linkConfig.UserPermitted(-1, 3) {
		t.Fatal("link permission granted admin rights")
	}
	// A channel sender must not inherit a user's permission via the compatibility From field.
	otherChannel.From.ID = 2
	otherChannel.Text = "https://example.com/channel"
	permissionUpdate(t, b, otherChannel, false)
	if len(*deleted) != 2 {
		t.Fatal("sender_chat inherited user permission")
	}
	anonymous.Text = "https://example.com/admin"
	permissionUpdate(t, b, anonymous, false)
	if len(*deleted) != 2 {
		t.Fatal("deleted anonymous administrator link")
	}
	anonymous.Text = "/revokelinks 2"
	permissionUpdate(t, b, anonymous, false)
	if b.linkConfig.UserPermitted(-1, 2) {
		t.Fatal("anonymous admin could not revoke permission")
	}
	// Enabling silent mode is per chat: these commands do nothing outside it.
	other := permissionMessage(9, 1, "/permitlinks 2")
	other.Chat.ID = -3
	permissionUpdate(t, b, other, false)
	if b.linkConfig.UserPermitted(-3, 2) || b.linkConfig.Silent(-3) {
		t.Fatal("permission command changed unrelated chat")
	}
}
func TestLinkPermissionTargetValidation(t *testing.T) {
	for _, args := range []string{"", "0", "-100", "@someone", "2 3", "2 https://example.com"} {
		if _, err := linkPermissionTarget(permissionMessage(1, 1, ""), args); err == nil {
			t.Fatalf("accepted target %q", args)
		}
	}
	m := permissionMessage(1, 1, "")
	m.ReplyToMessage = permissionMessage(2, 2, "")
	if target, err := linkPermissionTarget(m, ""); err != nil || target != 2 {
		t.Fatal("reply target failed", err)
	}
	if _, err := linkPermissionTarget(m, "3"); err == nil {
		t.Fatal("accepted ambiguous target")
	}
	m.ReplyToMessage.SenderChat = &telegram.Chat{ID: -1, Type: "supergroup"}
	if _, err := linkPermissionTarget(m, ""); err == nil {
		t.Fatal("accepted anonymous sender as a user")
	}
	m.ReplyToMessage.SenderChat = nil
	m.ReplyToMessage.From.IsBot = true
	if _, err := linkPermissionTarget(m, ""); err == nil {
		t.Fatal("accepted bot target")
	}
	m.ReplyToMessage.From.IsBot = false
	m.ReplyToMessage.Chat.ID = -2
	if _, err := linkPermissionTarget(m, ""); err == nil {
		t.Fatal("accepted cross-chat reply")
	}
}
func TestLinkPermissionSaveFailureRollback(t *testing.T) {
	s, err := loadLinkDeletionSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableSilent(-1); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserPermission(-1, 2, true); err != nil {
		t.Fatal(err)
	}
	if err = s.Set(-2, true); err != nil {
		t.Fatal(err)
	}
	restarted, err := loadLinkDeletionSettings(s.path)
	if err != nil || !restarted.UserPermitted(-1, 2) {
		t.Fatal("normal settings save lost permission", err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err = os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "settings.json")
	if err = s.SetUserPermission(-1, 3, true); err == nil || s.UserPermitted(-1, 3) {
		t.Fatal("failed grant changed permission")
	}
	if err = s.SetUserPermission(-1, 2, false); err == nil || !s.UserPermitted(-1, 2) {
		t.Fatal("failed revoke changed permission")
	}
	if err = s.SetUserPermission(-2, 2, true); err == nil {
		t.Fatal("granted outside silent mode")
	}
	if err = s.SetUserPermission(-1, -2, true); err == nil {
		t.Fatal("permitted a channel ID")
	}
}
