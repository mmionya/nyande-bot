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
	Kind          string
	Name          string
	MIME          string
	Data          []byte
	PreviewFileID string // Telegram sticker thumbnail, used only if rendering fails.
}

type mediaCache struct {
	mu       sync.Mutex
	maximum  int
	maxBytes int64
	bytes    int64
	ttl      time.Duration
	now      func() time.Time
	items    map[mediaKey][]cachedAttachment
	expires  map[mediaKey]time.Time
	order    []mediaKey
}

type mediaKey struct {
	ChatID    int64
	MessageID int
}

func newMediaCache(maximum int) *mediaCache {
	return &mediaCache{maximum: maximum, maxBytes: 64 << 20, ttl: 30 * time.Minute,
		now: time.Now, items: make(map[mediaKey][]cachedAttachment), expires: make(map[mediaKey]time.Time)}
}

func (c *mediaCache) Put(chatID int64, messages []telegram.Message, attachments []cachedAttachment) {
	if len(messages) == 0 || len(attachments) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneExpired(now)
	// Deliveries retain the attachment order, including a partially sent album.
	for index, message := range messages[:min(len(messages), len(attachments))] {
		key := mediaKey{ChatID: chatID, MessageID: message.MessageID}
		c.remove(key)
		size := int64(len(attachments[index].Data))
		if c.maximum <= 0 || size > c.maxBytes {
			continue
		}
		// Evict before copying: a large insertion must not transiently double
		// the cache's memory budget. Oversized files remain downloadable normally.
		for len(c.order) >= c.maximum || c.bytes+size > c.maxBytes {
			c.remove(c.order[0])
		}
		c.items[key] = cloneAttachments(attachments[index : index+1])
		c.bytes += size
		c.expires[key] = now.Add(c.ttl)
		c.order = append(c.order, key)
	}
}

func (c *mediaCache) remove(key mediaKey) {
	for _, item := range c.items[key] {
		c.bytes -= int64(len(item.Data))
	}
	delete(c.items, key)
	delete(c.expires, key)
	for i, existing := range c.order {
		if existing == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *mediaCache) pruneExpired(now time.Time) {
	for key, expires := range c.expires {
		if !now.Before(expires) {
			c.remove(key)
		}
	}
}

func (c *mediaCache) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpired(c.now())
}

func (c *mediaCache) Get(chatID int64, messageID int) []cachedAttachment {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpired(c.now())
	// A hit refreshes LRU order, but never extends the absolute lifetime.
	if _, exists := c.items[mediaKey{chatID, messageID}]; exists {
		key := mediaKey{chatID, messageID}
		for i, existing := range c.order {
			if existing == key {
				c.order = append(append(c.order[:i], c.order[i+1:]...), key)
				break
			}
		}
	}
	return cloneAttachments(c.items[mediaKey{ChatID: chatID, MessageID: messageID}])
}

func (c *mediaCache) Has(chatID int64, messageID int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpired(c.now())
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
