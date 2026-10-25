package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueuedRequestKeepsReceivedDay(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	received := testTime(t, s.Config, "2026-11-01 23:59").Add(59 * time.Second)
	processed := received.Add(2 * time.Second)
	for _, date := range []string{"2026-11-01", "2026-11-02"} {
		if _, err := s.Store.ImportPromos(ctx, date, []string{"CODE-" + date}, received); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Receive(ctx, Event{ID: "late-promo", UserID: 1, Action: "promo"}, received); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, processed); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='2026-11-01'") != 0 {
		t.Fatal("draw ignored pending events")
	}
	if _, err := s.ProcessOne(ctx, processed); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM promos WHERE day='2026-11-01' AND user_id=1") != 1 || scalar(t, s, "SELECT COUNT(*) FROM activity WHERE day='2026-11-01' AND user_id=1") != 1 {
		t.Fatal("request moved to next day")
	}
	if err := s.Receive(ctx, Event{ID: "late-check", UserID: 1, Action: "check:2026-11-01"}, received); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessOne(ctx, processed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverOne(ctx, processed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverOne(ctx, processed.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, processed); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draw_entries WHERE draw_id='2026-11-01'") != 1 {
		t.Fatal("late registration missing from snapshot")
	}
}

func TestFinalWaitsForLastDayEvents(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	received := testTime(t, s.Config, "2026-11-10 23:59")
	processed := received.Add(2 * time.Minute)
	if err := s.Receive(ctx, Event{ID: "last-check", UserID: 1, Action: "check:2026-11-10"}, received); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, processed); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='final'") != 0 {
		t.Fatal("final ignored pending last-day entry")
	}
	if _, err := s.ProcessOne(ctx, processed); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, processed); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT weight FROM draw_entries WHERE draw_id='final' AND user_id=1") != 1 {
		t.Fatal("last chance missing")
	}
}

func TestQueuedConsentBeforeDeadlineKeepsPrize(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-02 00:00")
	seedParticipants(t, s, 4, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Approve(ctx, s.Config, "2026-11-01", "admin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	var id, user, deadline int64
	if err := s.Store.DB.QueryRow("SELECT id,user_id,deadline FROM winners WHERE status='notified'").Scan(&id, &user, &deadline); err != nil {
		t.Fatal(err)
	}
	event(t, s, "claim", user, fmt.Sprintf("claim:%d", id), "", now)
	event(t, s, "name", user, "text", "Иван Иванов", now)
	if err := s.Receive(ctx, Event{ID: "consent", UserID: user, Action: fmt.Sprintf("consent:%d", id)}, time.Unix(deadline-1, 0)); err != nil {
		t.Fatal(err)
	}
	late := time.Unix(deadline+10, 0)
	if err := s.Store.ExpirePrizes(ctx, s.Config, late); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='expired'") != 0 {
		t.Fatal("prize expired with timely consent queued")
	}
	if _, err := s.ProcessOne(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, late); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed'") != 1 {
		t.Fatal("timely consent rejected")
	}
}

func TestBadPrizeMessageDoesNotForfeitPrize(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-02 00:00")
	seedParticipants(t, s, 4, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Approve(ctx, s.Config, "2026-11-01", "admin", now); err != nil {
		t.Fatal(err)
	}
	p.sendErr = &APIError{Code: 400}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected send error")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='expired'") != 0 || scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='pending' AND deadline IS NULL") != 1 {
		t.Fatal("API 400 forfeited prize")
	}
	h := (&HTTP{Service: s, AdminToken: "admin"}).Handler()
	r := httptest.NewRequest("GET", "/admin/messages/failed", nil)
	r.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var failures []struct {
		ID int64 `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &failures) != nil || len(failures) != 1 {
		t.Fatal("failed message not visible to admin")
	}
	r = httptest.NewRequest("POST", fmt.Sprintf("/admin/messages/%d/retry", failures[0].ID), strings.NewReader(`{"text":"Исправленное уведомление о призе"}`))
	r.Header.Set("Authorization", "Bearer admin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p.sendErr = nil
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(p.sent) != 1 || p.sent[0].Text != "Исправленное уведомление о призе" || len(p.sent[0].Buttons) == 0 {
		t.Fatal("repaired message lost claim button")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='notified' AND deadline IS NOT NULL") != 1 {
		t.Fatal("deadline not started after successful retry")
	}
}

func TestRepliesBypassBroadcastAndRespectChatLimit(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	seedParticipants(t, s, 1000, now)
	data, _ := json.Marshal(Message{Text: "broadcast"})
	if _, err := s.Store.DB.Exec("INSERT INTO outbox(user_id,payload,created_at,broadcast) SELECT id,?,?,1 FROM users", data, now.Unix()); err != nil {
		t.Fatal(err)
	}
	event(t, s, "help", 1, "help", "", now)
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(p.sent) != 1 || p.sent[0].Text == "broadcast" {
		t.Fatal("broadcast blocked direct reply")
	}
	for i := 0; i < 999; i++ {
		if work, err := s.DeliverOne(ctx, now); err != nil || !work {
			t.Fatalf("load delivery %d: %v", i, err)
		}
	}
	if work, err := s.DeliverOne(ctx, now); err != nil || work {
		t.Fatal("same chat sent twice within limit")
	}
	if work, err := s.DeliverOne(ctx, now.Add(2*time.Second)); err != nil || !work {
		t.Fatal("chat did not become eligible")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox WHERE sent_at IS NOT NULL") != 1001 {
		t.Fatal("load lost messages")
	}
}

type blockedSender struct {
	started chan struct{}
	release chan struct{}
	checks  atomic.Int32
}

func (p *blockedSender) IsMember(context.Context, int64, int64) (bool, error) {
	p.checks.Add(1)
	return true, nil
}
func (p *blockedSender) Send(ctx context.Context, _ int64, _ Message) error {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSlowDeliveryDoesNotStopInputWorker(t *testing.T) {
	s, _ := newTestService(t)
	p := &blockedSender{started: make(chan struct{}, 1), release: make(chan struct{})}
	s.Platform = p
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now()
	if err := s.Receive(ctx, Event{ID: "first", UserID: 1, Action: "start"}, now); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("sender did not start")
	}
	if err := s.Receive(ctx, Event{ID: "second", UserID: 2, Action: "start"}, now); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for scalar(t, s, "SELECT COUNT(*) FROM inbox WHERE processed_at IS NOT NULL") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("input stalled behind sender")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackupRestoresCommittedState(t *testing.T) {
	s, _ := newTestService(t)
	now := testTime(t, s.Config, "2026-11-01 12:00")
	ctx := context.Background()
	if _, err := s.Store.ImportPromos(ctx, "2026-11-01", []string{"BACKUP-CODE"}, now); err != nil {
		t.Fatal(err)
	}
	event(t, s, "backup", 1, "promo", "", now)
	path := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.Store.DB.Exec("VACUUM INTO ?", path); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.DB.Close()
	copyService := &Service{Store: restored, Config: s.Config, Platform: &fakePlatform{}}
	if scalar(t, copyService, "SELECT COUNT(*) FROM activity") != 1 || scalar(t, copyService, "SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL") != 1 {
		t.Fatal("backup lost committed state")
	}
	if work, err := copyService.ProcessOne(ctx, now); err != nil || work {
		t.Fatal("processed event replayed after restore")
	}
	if _, err := copyService.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
}

func TestCrashHelper(t *testing.T) {
	path := os.Getenv("BOT_TEST_CRASH_DB")
	if path == "" {
		return
	}
	store, err := OpenStore(path)
	if err != nil {
		os.Exit(20)
	}
	s := &Service{Store: store, Config: testConfig(t), Platform: &fakePlatform{}}
	now := testTime(t, s.Config, "2026-11-01 12:00")
	switch os.Getenv("BOT_TEST_CRASH_STAGE") {
	case "transaction":
		tx, err := store.DB.Begin()
		if err != nil {
			os.Exit(21)
		}
		if _, err = tx.Exec("INSERT INTO activity(day,user_id,partner,created_at) VALUES('2026-11-01',1,'partner',?)", now.Unix()); err != nil {
			os.Exit(22)
		}
	case "received", "processed":
		if err := s.Receive(context.Background(), Event{ID: "crash", UserID: 1, Action: "promo"}, now); err != nil {
			os.Exit(23)
		}
		if os.Getenv("BOT_TEST_CRASH_STAGE") == "processed" {
			if _, err := s.ProcessOne(context.Background(), now); err != nil {
				os.Exit(24)
			}
		}
	}
	os.Exit(17)
}

func TestAbruptExitPreservesTransactionsAndQueue(t *testing.T) {
	for _, stage := range []string{"transaction", "received", "processed"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crash.db")
			store, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			now := testTime(t, testConfig(t), "2026-11-01 12:00")
			if _, err = store.DB.Exec("INSERT INTO users(id,started_at,start_day) VALUES(1,?,'2026-11-01')", now.Unix()); err != nil {
				t.Fatal(err)
			}
			if _, err = store.ImportPromos(context.Background(), "2026-11-01", []string{"CRASH-CODE"}, now); err != nil {
				t.Fatal(err)
			}
			store.DB.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
			cmd.Env = append(os.Environ(), "BOT_TEST_CRASH_DB="+path, "BOT_TEST_CRASH_STAGE="+stage)
			if err = cmd.Run(); err == nil {
				t.Fatal("child did not exit abruptly")
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("unexpected child error: %v", err)
			}
			store, err = OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.DB.Close()
			s := &Service{Store: store, Config: testConfig(t), Platform: &fakePlatform{}}
			var integrity string
			if err = store.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatal("database damaged")
			}
			if stage == "transaction" {
				if scalar(t, s, "SELECT COUNT(*) FROM activity") != 0 {
					t.Fatal("uncommitted transaction survived")
				}
				return
			}
			if _, err = s.ProcessOne(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if scalar(t, s, "SELECT COUNT(*) FROM activity") != 1 || scalar(t, s, "SELECT COUNT(*) FROM outbox") != 1 {
				t.Fatal("crash lost or duplicated committed work")
			}
		})
	}
}
