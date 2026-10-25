package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Telegram struct {
	Token   string
	BaseURL string
	Client  *http.Client
}

func NewTelegram(token string) *Telegram {
	return &Telegram{Token: token, BaseURL: "https://api.telegram.org", Client: &http.Client{Timeout: 12 * time.Second}}
}

type APIError struct {
	Code       int
	RetryAfter int
}

func (e *APIError) Error() string { return fmt.Sprintf("telegram API status %d", e.Code) }

func (t *Telegram) call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/bot"+t.Token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram transport failed")
	}
	defer resp.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		ErrorCode  int             `json:"error_code"`
		Result     json.RawMessage `json:"result"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("invalid telegram response (HTTP %d)", resp.StatusCode)
	}
	if !envelope.OK || resp.StatusCode >= 400 {
		code := envelope.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Code: code, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

func (t *Telegram) IsMember(ctx context.Context, channel, user int64) (bool, error) {
	var member struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"`
	}
	err := t.call(ctx, "getChatMember", map[string]any{"chat_id": channel, "user_id": user}, &member)
	if err != nil {
		return false, err
	}
	return member.Status == "creator" || member.Status == "administrator" || member.Status == "member" || (member.Status == "restricted" && member.IsMember), nil
}

func (t *Telegram) Send(ctx context.Context, user int64, m Message) error {
	keyboard := make([][]map[string]string, 0, len(m.Buttons))
	for _, row := range m.Buttons {
		buttons := make([]map[string]string, 0, len(row))
		for _, b := range row {
			button := map[string]string{"text": b.Text}
			if b.Type == "link" {
				button["url"] = b.URL
			} else {
				button["callback_data"] = b.Payload
			}
			buttons = append(buttons, button)
		}
		keyboard = append(keyboard, buttons)
	}
	payload := map[string]any{"chat_id": user, "text": m.Text, "reply_markup": map[string]any{"inline_keyboard": keyboard}}
	method := "sendMessage"
	if m.ImageURL != "" {
		method = "sendPhoto"
		delete(payload, "text")
		payload["photo"] = m.ImageURL
		payload["caption"] = m.Text
	}
	return t.call(ctx, method, payload, nil)
}

func (t *Telegram) AnswerCallback(ctx context.Context, id string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": id}, nil)
}

func (t *Telegram) SetWebhook(ctx context.Context, url, secret string) error {
	return t.call(ctx, "setWebhook", map[string]any{"url": url, "secret_token": secret, "allowed_updates": []string{"message", "callback_query"}}, nil)
}

type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}
type telegramMessage struct {
	From telegramUser `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
}
type TelegramUpdate struct {
	UpdateID *int64           `json:"update_id"`
	Message  *telegramMessage `json:"message"`
	Callback *struct {
		ID      string           `json:"id"`
		From    telegramUser     `json:"from"`
		Message *telegramMessage `json:"message"`
		Data    string           `json:"data"`
	} `json:"callback_query"`
}

func (u TelegramUpdate) Event() (Event, bool) {
	if u.UpdateID == nil {
		return Event{}, false
	}
	e := Event{ID: strconv.FormatInt(*u.UpdateID, 10)}
	if u.Callback != nil {
		c := u.Callback
		if c.From.IsBot || c.From.ID <= 0 || c.Message == nil || c.Message.Chat.Type != "private" || c.Message.Chat.ID != c.From.ID {
			return Event{}, false
		}
		e.UserID = c.From.ID
		e.Action = c.Data
		return e, true
	}
	if u.Message == nil || u.Message.From.IsBot || u.Message.From.ID <= 0 || u.Message.Chat.Type != "private" || u.Message.Chat.ID != u.Message.From.ID {
		return Event{}, false
	}
	e.UserID = u.Message.From.ID
	e.Text = u.Message.Text
	e.Action = "text"
	if fields := strings.Fields(e.Text); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		command := strings.SplitN(fields[0], "@", 2)[0]
		switch command {
		case "/start":
			e.Action = "start"
		case "/stop":
			e.Action = "stop"
		case "/help":
			e.Action = "help"
		case "/cancel":
			e.Action = "cancel"
		}
	}
	return e, true
}
