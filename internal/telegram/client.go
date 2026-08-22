package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRequestAttempts = 4

type Client struct {
	token      string
	apiBaseURL string
	fileBase   string
	http       *http.Client
}

func New(token string) *Client {
	return &Client{
		token:      token,
		apiBaseURL: "https://api.telegram.org/bot" + token + "/",
		fileBase:   "https://api.telegram.org/file/bot" + token + "/",
		http: &http.Client{
			Timeout: 70 * time.Second,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}
}

type apiEnvelope[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.do(ctx, method, "application/json", data, result)
}

func (c *Client) do(ctx context.Context, method, contentType string, body []byte, result any) error {
	var last error
	for attempt := 1; attempt <= maxRequestAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+method, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", contentType)
		response, err := c.http.Do(request)
		if err != nil {
			last = sanitizeError(err, c.token)
			if attempt < maxRequestAttempts {
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
				continue
			}
			return last
		}
		responseData, readErr := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
		response.Body.Close()
		if readErr != nil {
			last = readErr
			if attempt < maxRequestAttempts {
				continue
			}
			return last
		}

		var raw apiEnvelope[json.RawMessage]
		if err := json.Unmarshal(responseData, &raw); err != nil {
			last = fmt.Errorf("Telegram API returned invalid JSON (HTTP %d)", response.StatusCode)
			if attempt < maxRequestAttempts && response.StatusCode >= 500 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return last
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 && raw.OK {
			if result == nil || len(raw.Result) == 0 || string(raw.Result) == "true" {
				return nil
			}
			if err := json.Unmarshal(raw.Result, result); err != nil {
				return fmt.Errorf("decode Telegram %s result: %w", method, err)
			}
			return nil
		}
		apiErr := &APIError{
			StatusCode: response.StatusCode, ErrorCode: raw.ErrorCode,
			Description: raw.Description, RetryAfter: raw.Parameters.RetryAfter,
		}
		last = apiErr
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		if !retryable || attempt == maxRequestAttempts {
			return apiErr
		}
		delay := time.Duration(apiErr.RetryAfter) * time.Second
		if delay <= 0 {
			delay = time.Duration(attempt) * time.Second
		}
		log.Printf("[telegram] temporary failure in %s, retry %d/%d in %s", method, attempt, maxRequestAttempts, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return last
}

func sanitizeError(err error, token string) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var result User
	err := c.call(ctx, "getMe", struct{}{}, &result)
	return result, err
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var result []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 50,
		"allowed_updates": []string{"message", "callback_query", "pre_checkout_query"},
	}, &result)
	return result, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int, markup any) (Message, error) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if replyTo != 0 {
		payload["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	var result Message
	err := c.call(ctx, "sendMessage", payload, &result)
	return result, err
}

func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text string, markup any) error {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}

func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, id, text, callbackURL string, alert bool) error {
	payload := map[string]any{"callback_query_id": id, "cache_time": 0}
	if text != "" {
		payload["text"] = text
		payload["show_alert"] = alert
	}
	if callbackURL != "" {
		payload["url"] = callbackURL
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
}

func (c *Client) SendInvoice(ctx context.Context, chatID int64, title, description, payload string, amount int) error {
	return c.call(ctx, "sendInvoice", map[string]any{
		"chat_id": chatID, "title": title, "description": description,
		"payload": payload, "currency": "XTR",
		"prices": []map[string]any{{"label": title, "amount": amount}},
	}, nil)
}

func (c *Client) AnswerPreCheckout(ctx context.Context, id string, ok bool, errorMessage string) error {
	payload := map[string]any{"pre_checkout_query_id": id, "ok": ok}
	if errorMessage != "" {
		payload["error_message"] = errorMessage
	}
	return c.call(ctx, "answerPreCheckoutQuery", payload, nil)
}

func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	var result ChatMember
	err := c.call(ctx, "getChatMember", map[string]any{"chat_id": chatID, "user_id": userID}, &result)
	return result, err
}

func (c *Client) SetCommands(ctx context.Context, commands []map[string]string) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var result File
	err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &result)
	return result, err
}

func (c *Client) GetUserProfilePhotos(ctx context.Context, userID int64, limit int) (UserProfilePhotos, error) {
	if limit <= 0 {
		limit = 1
	}
	var result UserProfilePhotos
	err := c.call(ctx, "getUserProfilePhotos", map[string]any{
		"user_id": userID, "offset": 0, "limit": limit,
	}, &result)
	return result, err
}

func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, string, error) {
	file, err := c.GetFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	if file.FileSize > maxBytes && maxBytes > 0 {
		return nil, "", fmt.Errorf("Telegram file is too large: %d bytes", file.FileSize)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileBase+file.FilePath, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", sanitizeError(err, c.token)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Telegram file HTTP %d", response.StatusCode)
	}
	limit := maxBytes
	if limit <= 0 {
		limit = 100 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("Telegram file exceeds %d bytes", limit)
	}
	return data, file.FilePath, nil
}

func (c *Client) SendUpload(ctx context.Context, chatID int64, upload Upload, replyTo int) (Message, error) {
	field := upload.Kind
	if field != "photo" && field != "video" && field != "animation" && field != "document" {
		field = "document"
	}
	method := "send" + strings.ToUpper(field[:1]) + field[1:]
	if len(upload.Data) == 0 {
		payload := map[string]any{"chat_id": chatID, field: upload.URL}
		if upload.Caption != "" {
			payload["caption"] = upload.Caption
		}
		if field == "video" {
			payload["supports_streaming"] = true
		}
		if replyTo != 0 {
			payload["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
		}
		var result Message
		err := c.call(ctx, method, payload, &result)
		return result, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if upload.Caption != "" {
		_ = writer.WriteField("caption", upload.Caption)
	}
	if field == "video" {
		_ = writer.WriteField("supports_streaming", "true")
	}
	if replyTo != 0 {
		value, _ := json.Marshal(map[string]any{"message_id": replyTo, "allow_sending_without_reply": true})
		_ = writer.WriteField("reply_parameters", string(value))
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, safeFilename(upload.Name)))
	header.Set("Content-Type", first(upload.MIME, "application/octet-stream"))
	part, err := writer.CreatePart(header)
	if err != nil {
		return Message{}, err
	}
	if _, err := part.Write(upload.Data); err != nil {
		return Message{}, err
	}
	if err := writer.Close(); err != nil {
		return Message{}, err
	}
	var result Message
	err = c.do(ctx, method, writer.FormDataContentType(), body.Bytes(), &result)
	return result, err
}

// SendAnimation sends an animation by URL through Telegram's native
// sendAnimation method. The tomato tool now uploads a generated GIF instead,
// but this method remains available for callers and older tests.
func (c *Client) SendAnimation(ctx context.Context, chatID int64, animationURL string, replyTo int) (Message, error) {
	payload := map[string]any{"chat_id": chatID, "animation": animationURL}
	if replyTo != 0 {
		payload["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var result Message
	err := c.call(ctx, "sendAnimation", payload, &result)
	return result, err
}

func (c *Client) SendMediaGroup(ctx context.Context, chatID int64, uploads []Upload, replyTo int) ([]Message, error) {
	if len(uploads) < 2 || len(uploads) > 10 {
		return nil, fmt.Errorf("media group must contain 2..10 items")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if replyTo != 0 {
		value, _ := json.Marshal(map[string]any{"message_id": replyTo, "allow_sending_without_reply": true})
		_ = writer.WriteField("reply_parameters", string(value))
	}
	media := make([]map[string]any, 0, len(uploads))
	for index, upload := range uploads {
		kind := upload.Kind
		if kind != "photo" && kind != "video" && kind != "document" {
			kind = "document"
		}
		item := map[string]any{"type": kind}
		if upload.Caption != "" {
			item["caption"] = upload.Caption
		}
		if len(upload.Data) == 0 {
			item["media"] = upload.URL
		} else {
			attachment := fmt.Sprintf("file%d", index)
			item["media"] = "attach://" + attachment
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, attachment, safeFilename(upload.Name)))
			header.Set("Content-Type", first(upload.MIME, "application/octet-stream"))
			part, err := writer.CreatePart(header)
			if err != nil {
				return nil, err
			}
			if _, err := part.Write(upload.Data); err != nil {
				return nil, err
			}
		}
		media = append(media, item)
	}
	encoded, err := json.Marshal(media)
	if err != nil {
		return nil, err
	}
	_ = writer.WriteField("media", string(encoded))
	if err := writer.Close(); err != nil {
		return nil, err
	}
	var result []Message
	err = c.do(ctx, "sendMediaGroup", writer.FormDataContentType(), body.Bytes(), &result)
	return result, err
}

func safeFilename(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(name, "\r", ""), "\n", ""))
	name = strings.ReplaceAll(name, `"`, "_")
	if name == "" {
		return "media.bin"
	}
	return name
}

func first(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func EncodeQuery(values map[string]string) string {
	query := url.Values{}
	for key, value := range values {
		query.Set(key, value)
	}
	return query.Encode()
}
