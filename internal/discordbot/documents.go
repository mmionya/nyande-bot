package discordbot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/documents"
	"github.com/mmionya/nyande-bot/resources"
)

func documentAttachments(message *discordgo.Message) []*discordgo.MessageAttachment {
	var result []*discordgo.MessageAttachment
	if message == nil {
		return result
	}
	for _, attachment := range message.Attachments {
		if attachment != nil && documents.Supported(attachment.Filename, attachment.ContentType) {
			result = append(result, attachment)
		}
	}
	return result
}

func (b *Bot) collectDocuments(ctx context.Context, session *discordgo.Session, message *discordgo.Message) ([]documents.Document, error) {
	attachments := documentAttachments(message)
	if len(attachments) == 0 {
		replied := message.ReferencedMessage
		if replied == nil && message.MessageReference != nil && message.MessageReference.MessageID != "" {
			var err error
			replied, err = session.ChannelMessage(message.ChannelID, message.MessageReference.MessageID, discordgo.WithContext(ctx))
			if err != nil {
				return nil, errors.New(resources.Get("documents.error.download"))
			}
		}
		attachments = documentAttachments(replied)
	}
	if len(attachments) > documents.MaxDocuments {
		return nil, errors.New(resources.Get("documents.error.too_many"))
	}
	var result []documents.Document
	imageBudget := 0
	if b.cfg.LLMVision {
		imageBudget = 12
	}
	for index, attachment := range attachments {
		data, err := b.downloadDocument(ctx, session, attachment)
		if err != nil {
			return nil, err
		}
		// Reserve images for every document, including scans with no text layer.
		imageLimit := min(6, imageBudget/(len(attachments)-index))
		doc, err := documents.Read(ctx, attachment.Filename, attachment.ContentType, data, imageLimit)
		if err != nil {
			return nil, errors.New(resources.Format("documents.error.read", map[string]any{"name": attachment.Filename, "error": err}))
		}
		if b.cfg.LLMVision && imageBudget == 0 {
			doc.Warning = resources.Get("documents.warning.image_budget")
		}
		imageBudget -= len(doc.Images)
		result = append(result, doc)
	}
	return result, nil
}

func discordAttachmentURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		(u.Hostname() == "cdn.discordapp.com" || u.Hostname() == "media.discordapp.net") && strings.HasPrefix(u.Path, "/attachments/")
}

func (b *Bot) downloadDocument(ctx context.Context, session *discordgo.Session, attachment *discordgo.MessageAttachment) ([]byte, error) {
	limit := int64(documents.MaxFileSize)
	if b.cfg.MaxFileSize > 0 {
		limit = min(limit, b.cfg.MaxFileSize)
	}
	if int64(attachment.Size) > limit {
		return nil, documents.ErrTooLarge
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, attachment.URL, nil)
	if err != nil || !discordAttachmentURL(request.URL) {
		return nil, errors.New(resources.Get("documents.error.download"))
	}
	// A separate client avoids forwarding the Discord API authorization header.
	client := &http.Client{Timeout: 30 * time.Second, Transport: session.Client.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !discordAttachmentURL(req.URL) {
				return errors.New(resources.Get("documents.error.download"))
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New(resources.Get("documents.error.download"))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New(resources.Get("documents.error.download"))
	}
	if response.ContentLength > limit {
		return nil, documents.ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, errors.New(resources.Get("documents.error.download"))
	}
	if int64(len(data)) > limit {
		return nil, documents.ErrTooLarge
	}
	return data, nil
}
