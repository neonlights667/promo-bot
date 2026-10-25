package bot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramMembershipAndButtons(t *testing.T) {
	status := "member"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/bottest/getChatMember":
			if body["chat_id"].(float64) != -100 || body["user_id"].(float64) != 42 {
				t.Error("wrong membership target")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"status": status, "is_member": false}})
		case "/bottest/sendMessage":
			keyboard := body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
			button := keyboard[0].([]any)[0].(map[string]any)
			if button["callback_data"] != "promo" {
				t.Error("wrong callback payload")
			}
			w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			t.Error("unexpected method")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := NewTelegram("test")
	client.BaseURL = server.URL
	for _, test := range []struct {
		status string
		member bool
	}{{"member", true}, {"administrator", true}, {"creator", true}, {"left", false}, {"kicked", false}, {"restricted", false}} {
		status = test.status
		member, err := client.IsMember(context.Background(), -100, 42)
		if err != nil || member != test.member {
			t.Fatalf("status %s => %v, %v", status, member, err)
		}
	}
	if err := client.Send(context.Background(), 42, Message{Text: "Промокод", Buttons: menu()}); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramRateLimitAndTokenRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"ok":false,"error_code":429,"parameters":{"retry_after":45}}`))
	}))
	defer server.Close()
	client := NewTelegram("private-token")
	client.BaseURL = server.URL
	err := client.Send(context.Background(), 1, Message{Text: "test"})
	var api *APIError
	if !errors.As(err, &api) || api.Code != 429 || api.RetryAfter != 45 {
		t.Fatalf("unexpected error: %v", err)
	}
	server.Close()
	err = client.Send(context.Background(), 1, Message{Text: "test"})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("transport error exposed token")
	}
}

func TestTelegramIgnoresGroupsAndMapsCommands(t *testing.T) {
	for _, test := range []struct {
		raw, action string
		accepted    bool
	}{
		{`{"update_id":1,"message":{"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/start"}}`, "start", true},
		{`{"update_id":2,"message":{"from":{"id":42},"chat":{"id":-1,"type":"group"},"text":"/start"}}`, "", false},
		{`{"update_id":3,"callback_query":{"id":"abc","from":{"id":42},"data":"promo","message":{"chat":{"id":42,"type":"private"}}}}`, "promo", true},
		{`{"message":{"from":{"id":42},"chat":{"id":42,"type":"private"}}}`, "", false},
	} {
		var update TelegramUpdate
		if err := json.Unmarshal([]byte(test.raw), &update); err != nil {
			t.Fatal(err)
		}
		e, ok := update.Event()
		if ok != test.accepted || e.Action != test.action {
			t.Fatalf("unexpected event %+v %v", e, ok)
		}
	}
}
