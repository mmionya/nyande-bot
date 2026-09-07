package bot

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

type State struct {
	StartedAt time.Time

	MessagesTotal        atomic.Int64
	CommandsUsed         atomic.Int64
	LLMCalls             atomic.Int64
	LLMErrors            atomic.Int64
	MediaTotal           atomic.Int64
	MediaErrors          atomic.Int64
	TikTokDownloads      atomic.Int64
	InstagramDownloads   atomic.Int64
	TwitterDownloads     atomic.Int64
	RedditDownloads      atomic.Int64
	YouTubeDownloads     atomic.Int64
	XiaohongshuDownloads atomic.Int64
	PinterestDownloads   atomic.Int64

	chatMu      sync.Mutex
	uniqueChats map[int64]struct{}
	locks       map[int64]*sync.Mutex
}

func NewState() *State {
	return &State{
		StartedAt: time.Now(), uniqueChats: make(map[int64]struct{}),
		locks: make(map[int64]*sync.Mutex),
	}
}

func (s *State) TrackChat(chatID int64) {
	s.chatMu.Lock()
	s.uniqueChats[chatID] = struct{}{}
	s.chatMu.Unlock()
}

func (s *State) UniqueChats() int {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	return len(s.uniqueChats)
}

func (s *State) ChatLock(chatID int64) *sync.Mutex {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	lock := s.locks[chatID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[chatID] = lock
	}
	return lock
}

type cachedAttachment struct {
	Kind string
	Name string
	MIME string
	Data []byte
}

type mediaCache struct {
	mu      sync.Mutex
	maximum int
	items   map[mediaKey][]cachedAttachment
	order   []mediaKey
}

type mediaKey struct {
	ChatID    int64
	MessageID int
}

func newMediaCache(maximum int) *mediaCache {
	return &mediaCache{maximum: maximum, items: make(map[mediaKey][]cachedAttachment)}
}

func (c *mediaCache) Put(chatID int64, messages []telegram.Message, attachments []cachedAttachment) {
	if len(messages) == 0 || len(attachments) == 0 {
		return
	}
	copied := cloneAttachments(attachments)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, message := range messages {
		key := mediaKey{ChatID: chatID, MessageID: message.MessageID}
		if _, exists := c.items[key]; !exists {
			c.order = append(c.order, key)
		}
		c.items[key] = copied
	}
	for len(c.order) > c.maximum {
		key := c.order[0]
		c.order = c.order[1:]
		delete(c.items, key)
	}
}

func (c *mediaCache) Get(chatID int64, messageID int) []cachedAttachment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneAttachments(c.items[mediaKey{ChatID: chatID, MessageID: messageID}])
}

func (c *mediaCache) Has(chatID int64, messageID int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.items[mediaKey{ChatID: chatID, MessageID: messageID}]
	return exists
}

func cloneAttachments(source []cachedAttachment) []cachedAttachment {
	result := make([]cachedAttachment, len(source))
	for index, attachment := range source {
		result[index] = attachment
		result[index].Data = append([]byte(nil), attachment.Data...)
	}
	return result
}

func llmImages(attachments []cachedAttachment) []llm.Image {
	images := make([]llm.Image, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Kind == "photo" {
			images = append(images, llm.Image{MIME: attachment.MIME, Data: attachment.Data})
		}
	}
	return images
}
