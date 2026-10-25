package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDailyDrawWaitsForRegistrationNotification(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	drawTime := testTime(t, s.Config, "2026-11-02 00:00")
	event(t, s, "register", 1, "check:2026-11-01", "", now)
	if scalar(t, s, "SELECT COUNT(*) FROM entry_confirmations") != 0 {
		t.Fatal("queued response counted as sent")
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, drawTime); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='2026-11-01'") != 0 {
		t.Fatal("draw ran before confirmation delivery")
	}
	p.sendErr = &APIError{Code: 429, RetryAfter: 5}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected temporary delivery error")
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, drawTime); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='2026-11-01'") != 0 {
		t.Fatal("retrying response did not delay draw")
	}
	p.sendErr = nil
	if _, err := s.DeliverOne(ctx, drawTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, drawTime); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM entry_confirmations") != 1 || scalar(t, s, "SELECT COUNT(*) FROM draw_entries WHERE draw_id='2026-11-01'") != 1 {
		t.Fatal("confirmed participant missing")
	}
}

func TestFailedRegistrationNotificationExcludesDailyEntry(t *testing.T) {
	for _, code := range []int{400, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s, p := newTestService(t)
			ctx := context.Background()
			now := testTime(t, s.Config, "2026-11-01 12:00")
			event(t, s, "register", 1, "check:2026-11-01", "", now)
			p.sendErr = &APIError{Code: code}
			if _, err := s.DeliverOne(ctx, now); err == nil {
				t.Fatal("expected failed notification")
			}
			if err := s.Store.PrepareDraws(ctx, s.Config, now.AddDate(0, 0, 1)); err != nil {
				t.Fatal(err)
			}
			if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='2026-11-01'") != 1 || scalar(t, s, "SELECT COUNT(*) FROM draw_entries WHERE draw_id='2026-11-01'") != 0 {
				t.Fatal("unconfirmed participant entered daily draw")
			}
			if scalar(t, s, "SELECT COUNT(*) FROM activity") != 1 {
				t.Fatal("registration lost its final chance")
			}
			h := (&HTTP{Service: s, AdminToken: "admin"}).Handler()
			r := httptest.NewRequest("GET", "/admin/stats", nil)
			r.Header.Set("Authorization", "Bearer admin")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var stats struct {
				Days []struct {
					Participants int `json:"participants"`
				} `json:"days"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil || len(stats.Days) != 10 || stats.Days[0].Participants != 0 {
				t.Fatal("unconfirmed entry counted as participant")
			}
		})
	}
}

func TestRetriedRegistrationNotificationConfirmsEntry(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	event(t, s, "register", 1, "check:2026-11-01", "", now)
	p.sendErr = &APIError{Code: 400}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected send error")
	}
	var id int64
	if err := s.Store.DB.QueryRow("SELECT id FROM outbox WHERE failed_at IS NOT NULL").Scan(&id); err != nil {
		t.Fatal(err)
	}
	h := (&HTTP{Service: s, AdminToken: "admin"}).Handler()
	r := httptest.NewRequest("POST", fmt.Sprintf("/admin/messages/%d/retry", id), strings.NewReader(`{"text":"Заявка зарегистрирована"}`))
	r.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p.sendErr = nil
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM entry_confirmations") != 1 {
		t.Fatal("retry lost registration metadata")
	}
	event(t, s, "repeat", 1, "enter", "", now)
	if _, err := s.DeliverOne(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM entries") != 1 || scalar(t, s, "SELECT COUNT(*) FROM entry_confirmations") != 1 || scalar(t, s, "SELECT COUNT(*) FROM activity") != 1 {
		t.Fatal("repeated confirmation duplicated participation")
	}
}

func TestRepeatedCheckCanDeliverMissingConfirmation(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	event(t, s, "register", 1, "check:2026-11-01", "", now)
	p.sendErr = &APIError{Code: 403}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected send error")
	}
	event(t, s, "again", 1, "check:2026-11-01", "", now.Add(time.Second))
	p.sendErr = nil
	if _, err := s.DeliverOne(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM entry_confirmations") != 1 {
		t.Fatal("repeat response did not confirm original entry")
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draw_entries WHERE draw_id='2026-11-01'") != 1 {
		t.Fatal("recovered user missing from draw")
	}
}
