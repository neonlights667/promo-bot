package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePlatform struct {
	member   bool
	checkErr error
	sendErr  error
	sent     []Message
	checks   int
}

func (p *fakePlatform) IsMember(context.Context, int64, int64) (bool, error) {
	p.checks++
	return p.member, p.checkErr
}
func (p *fakePlatform) Send(_ context.Context, _ int64, m Message) error {
	if p.sendErr != nil {
		return p.sendErr
	}
	p.sent = append(p.sent, m)
	return nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	c := Config{Timezone: "Europe/Moscow", Rules: "Правила акции", Consent: "Согласен на передачу имени партнеру", GrandPrize: "Два билета на концерт", ReserveCount: 3, ClaimHours: 24}
	for i := 1; i <= 10; i++ {
		c.Days = append(c.Days, Day{Date: fmt.Sprintf("2026-11-%02d", i), Partner: fmt.Sprintf("Партнер %d", i), ChannelID: int64(-100000 - i), URL: "https://t.me/partner", PromoUntil: "30.11.2026", PromoUsage: "example.com", Prizes: []string{"Подарок"}, ClaimInstruction: "Дождись сообщения организатора"})
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}
func newTestService(t *testing.T) (*Service, *fakePlatform) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	p := &fakePlatform{member: true}
	return &Service{Store: store, Config: testConfig(t), Platform: p}, p
}
func testTime(t *testing.T, c Config, date string) time.Time {
	t.Helper()
	now, err := time.ParseInLocation("2006-01-02 15:04", date, c.Location)
	if err != nil {
		t.Fatal(err)
	}
	return now
}
func scalar(t *testing.T, s *Service, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func event(t *testing.T, s *Service, id string, user int64, action, text string, now time.Time) {
	t.Helper()
	if err := s.Receive(context.Background(), Event{ID: id, UserID: user, Action: action, Text: text}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessOne(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}

func TestPromoAndEntryShareOneChance(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	if _, err := s.Store.ImportPromos(ctx, "2026-11-01", []string{"CODE1", "CODE2"}, now); err != nil {
		t.Fatal(err)
	}
	event(t, s, "1", 42, "start", "", now)
	event(t, s, "2", 42, "promo", "", now)
	event(t, s, "3", 42, "promo", "", now)
	event(t, s, "4", 42, "check:2026-11-01", "", now)
	event(t, s, "5", 42, "check:2026-11-01", "", now)
	if n := scalar(t, s, "SELECT COUNT(*) FROM promos WHERE user_id=42"); n != 1 {
		t.Fatalf("issued %d promos", n)
	}
	if n := scalar(t, s, "SELECT COUNT(*) FROM activity WHERE user_id=42"); n != 1 {
		t.Fatalf("got %d chances", n)
	}
	if n := scalar(t, s, "SELECT COUNT(*) FROM entries WHERE user_id=42"); n != 1 {
		t.Fatalf("got %d entries", n)
	}
	if n := scalar(t, s, "SELECT COUNT(*) FROM promo_requests"); n != 2 {
		t.Fatalf("requests=%d", n)
	}
	if p.checks != 2 {
		t.Fatalf("checks=%d", p.checks)
	}
	event(t, s, "5", 42, "check:2026-11-01", "", now)
	if n := scalar(t, s, "SELECT COUNT(*) FROM outbox"); n != 5 {
		t.Fatalf("duplicate event created reply: %d", n)
	}
	next := now.AddDate(0, 0, 1)
	event(t, s, "6", 42, "check:2026-11-02", "", next)
	if n := scalar(t, s, "SELECT COUNT(*) FROM activity WHERE user_id=42"); n != 2 {
		t.Fatalf("next day chances=%d", n)
	}
}

func TestNoCodesDoesNotBlockEntry(t *testing.T) {
	s, p := newTestService(t)
	now := testTime(t, s.Config, "2026-11-01 12:00")
	event(t, s, "1", 1, "promo", "", now)
	if n := scalar(t, s, "SELECT COUNT(*) FROM activity"); n != 0 {
		t.Fatal("empty inventory gave a chance")
	}
	p.member = false
	for i := 2; i <= 4; i++ {
		event(t, s, fmt.Sprint(i), 1, "check:2026-11-01", "", now)
	}
	if n := scalar(t, s, "SELECT COUNT(*) FROM entries"); n != 0 {
		t.Fatal("unsubscribed user registered")
	}
	p.member = true
	p.checkErr = errors.New("API unavailable")
	event(t, s, "5", 1, "check:2026-11-01", "", now)
	if n := scalar(t, s, "SELECT COUNT(*) FROM entries"); n != 0 {
		t.Fatal("API failure registered user")
	}
	p.checkErr = nil
	event(t, s, "6", 1, "check:2026-11-01", "", now)
	if n := scalar(t, s, "SELECT COUNT(*) FROM entries"); n != 1 {
		t.Fatal("inventory blocked entry")
	}
}

func TestStaleButtonsAndTimezone(t *testing.T) {
	s, p := newTestService(t)
	now := time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)
	event(t, s, "1", 1, "check:2026-11-01", "", now)
	if p.checks != 0 || scalar(t, s, "SELECT COUNT(*) FROM entries") != 0 {
		t.Fatal("yesterday's check registered user")
	}
	event(t, s, "2", 1, "check:2026-11-02", "", now)
	if scalar(t, s, "SELECT COUNT(*) FROM entries WHERE day='2026-11-02'") != 1 {
		t.Fatal("wrong timezone boundary")
	}
	event(t, s, "3", 1, "promo", "", now.AddDate(0, 0, 20))
	if scalar(t, s, "SELECT COUNT(*) FROM promo_requests") != 0 {
		t.Fatal("accepted request outside campaign")
	}
}

func TestConcurrentPromoAllocationAndRollback(t *testing.T) {
	s, _ := newTestService(t)
	now := testTime(t, s.Config, "2026-11-01 12:00")
	ctx := context.Background()
	d := s.Config.Days[0]
	codes := make([]string, 10)
	for i := range codes {
		codes[i] = fmt.Sprintf("CODE%02d", i)
	}
	if _, err := s.Store.ImportPromos(ctx, d.Date, codes, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for user := int64(1); user <= 20; user++ {
		for repeat := 0; repeat < 2; repeat++ {
			wg.Add(1)
			go func(user int64) {
				defer wg.Done()
				errs <- s.Store.Transaction(ctx, func(tx *sql.Tx) error {
					if _, err := tx.Exec("INSERT INTO users(id,started_at,start_day) VALUES(?,?,?) ON CONFLICT DO NOTHING", user, now.Unix(), d.Date); err != nil {
						return err
					}
					_, err := givePromo(tx, user, d, now)
					return err
				})
			}(user)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, s, "SELECT COUNT(*) FROM promos WHERE user_id IS NOT NULL") != 10 || scalar(t, s, "SELECT COUNT(DISTINCT user_id) FROM promos") != 10 || scalar(t, s, "SELECT COUNT(*) FROM activity") != 10 {
		t.Fatal("codes or chances duplicated")
	}
	if _, err := s.Store.ImportPromos(ctx, d.Date, []string{"NEW", ""}, now); err == nil {
		t.Fatal("invalid import accepted")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM promos WHERE code='NEW'") != 0 {
		t.Fatal("partial import persisted")
	}
	if _, err := s.Store.ImportPromos(ctx, s.Config.Days[1].Date, []string{codes[0]}, now); err == nil {
		t.Fatal("code reused across partners")
	}
}

func seedParticipants(t *testing.T, s *Service, count int, now time.Time) {
	t.Helper()
	err := s.Store.Transaction(context.Background(), func(tx *sql.Tx) error {
		for user := 1; user <= count; user++ {
			if _, err := tx.Exec("INSERT INTO users(id,started_at,start_day) VALUES(?,?,?)", user, now.Unix(), s.Config.Days[0].Date); err != nil {
				return err
			}
			for day := 0; day < (user-1)%10+1; day++ {
				if err := addActivity(tx, int64(user), s.Config.Days[day], now); err != nil {
					return err
				}
			}
			if _, err := register(tx, int64(user), s.Config.Days[0], now); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO entry_confirmations(day,user_id,confirmed_at) VALUES(?,?,?)", s.Config.Days[0].Date, user, now.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDrawSnapshotsApprovalAndReserves(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-11 00:00")
	seedParticipants(t, s, 15, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draws WHERE id='final'") != 0 {
		t.Fatal("final drawn early")
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM draw_entries WHERE draw_id='final'") != 15 {
		t.Fatal("snapshot incomplete")
	}
	if scalar(t, s, "SELECT weight FROM draw_entries WHERE draw_id='final' AND user_id=10") != 10 {
		t.Fatal("weights lost")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE draw_id='final' AND status='pending'") != 5 {
		t.Fatal("expected five main winners")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE draw_id='final' AND status='reserve'") != 3 {
		t.Fatal("missing reserves")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox") != 0 {
		t.Fatal("notifications before approval")
	}
	var ranks string
	if err := s.Store.DB.QueryRow("SELECT GROUP_CONCAT(user_id) FROM winners WHERE draw_id='final' ORDER BY rank").Scan(&ranks); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	var repeated string
	s.Store.DB.QueryRow("SELECT GROUP_CONCAT(user_id) FROM winners WHERE draw_id='final' ORDER BY rank").Scan(&repeated)
	if ranks != repeated {
		t.Fatal("draw rerolled")
	}
	for i := 0; i < 2; i++ {
		if err := s.Store.Approve(ctx, s.Config, "final", "admin", now); err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox") != 5 {
		t.Fatal("approval duplicated messages")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE draw_id='final' AND status='expired'") != 0 {
		t.Fatal("deadline started before delivery")
	}
	for i := 0; i < 5; i++ {
		if _, err := s.DeliverOne(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.sent) != 5 {
		t.Fatal("notifications missing")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE draw_id='final' AND status='expired'") != 5 {
		t.Fatal("expired prizes not closed")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE draw_id='final' AND status='pending'") != 3 {
		t.Fatal("reserves not promoted")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox") != 8 {
		t.Fatal("reserve notifications missing")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox") != 8 {
		t.Fatal("reserve notified twice")
	}
}

func TestClaimRequiresOwnerNameAndConsent(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-02 00:00")
	seedParticipants(t, s, 5, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Approve(ctx, s.Config, "2026-11-01", "admin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	var id, user int64
	if err := s.Store.DB.QueryRow("SELECT id,user_id FROM winners WHERE status='notified'").Scan(&id, &user); err != nil {
		t.Fatal(err)
	}
	action := fmt.Sprintf("claim:%d", id)
	consent := fmt.Sprintf("consent:%d", id)
	event(t, s, "1", 999, action, "", now)
	event(t, s, "2", 999, consent, "", now)
	event(t, s, "3", user, consent, "", now)
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed'") != 0 {
		t.Fatal("claim without owner or name succeeded")
	}
	event(t, s, "4", user, action, "", now)
	event(t, s, "5", user, "text", "Иван", now)
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE full_name IS NOT NULL") != 0 {
		t.Fatal("invalid name accepted")
	}
	event(t, s, "6", user, "text", "Иван Иванов", now)
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed'") != 0 {
		t.Fatal("name counted as consent")
	}
	event(t, s, "7", user, consent, "", now)
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed' AND consent_at IS NOT NULL") != 1 {
		t.Fatal("claim not confirmed")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed'") != 1 {
		t.Fatal("claimed prize reassigned")
	}
}

func TestDeliveryRetryAndRestart(t *testing.T) {
	s, p := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	event(t, s, "1", 1, "start", "", now)
	p.sendErr = &APIError{Code: 429, RetryAfter: 30}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected retry")
	}
	p.sendErr = nil
	if work, err := s.DeliverOne(ctx, now.Add(29*time.Second)); err != nil || work {
		t.Fatal("retry_after ignored")
	}
	if _, err := s.DeliverOne(ctx, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent) != 1 {
		t.Fatal("retry not delivered")
	}
	path := filepath.Join(t.TempDir(), "restart.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	if err = s.Receive(ctx, Event{ID: "restart", UserID: 5, Action: "start"}, now); err != nil {
		t.Fatal(err)
	}
	store.DB.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s.Store = store
	if _, err = s.ProcessOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox") != 1 {
		t.Fatal("event lost on restart")
	}
	if err = s.Receive(ctx, Event{ID: "restart", UserID: 5, Action: "start"}, now); err != nil {
		t.Fatal(err)
	}
	if work, err := s.ProcessOne(ctx, now); err != nil || work {
		t.Fatal("duplicate reprocessed after restart")
	}
}

func TestHTTPAuthenticationExportAndBroadcast(t *testing.T) {
	s, _ := newTestService(t)
	now := testTime(t, s.Config, "2026-11-02 00:00")
	ctx := context.Background()
	seedParticipants(t, s, 6, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	h := (&HTTP{Service: s, WebhookSecret: "secret", AdminToken: "admin"}).Handler()
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/admin/stats", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("admin endpoint is public")
	}
	if w := request("POST", "/webhook", `{"update_id":1}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatal("webhook accepts forged updates")
	}
	w := request("GET", "/admin/draws/2026-11-01/export.csv", "", "Bearer admin")
	if w.Code != 200 || len(strings.Split(strings.TrimSpace(w.Body.String()), "\n")) != 7 {
		t.Fatalf("incomplete export: %s", w.Body.String())
	}
	if w = request("POST", "/admin/broadcasts", `{"id":"news","text":"Новости"}`, "Bearer admin"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	request("POST", "/admin/broadcasts", `{"id":"news","text":"Новости"}`, "Bearer admin")
	if scalar(t, s, "SELECT COUNT(*) FROM outbox WHERE broadcast=1") != 6 {
		t.Fatal("duplicate broadcast")
	}
	event(t, s, "stop", 1, "stop", "", now)
	for i := 0; i < 6; i++ {
		s.DeliverOne(ctx, now)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox WHERE broadcast=1 AND failed_at IS NOT NULL") != 1 {
		t.Fatal("opt-out ignored")
	}
	w = request("GET", "/admin/stats", "", "Bearer admin")
	var stats map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats["users"].(float64) != 6 {
		t.Fatal("invalid stats")
	}
}

func TestConfigLoadAndValidation(t *testing.T) {
	c := testConfig(t)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "campaign.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Location == nil {
		t.Fatal("timezone was not loaded")
	}
	if _, ok := loaded.Current(time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("valid day not found")
	}
	loaded.Days[1].Date = loaded.Days[0].Date
	if loaded.Validate() == nil {
		t.Fatal("duplicate day accepted")
	}
}

func TestPickerUniqueWinners(t *testing.T) {
	pool := []Candidate{{UserID: 1, Weight: 10}, {UserID: 2, Weight: 1}, {UserID: 3, Weight: 7}}
	for i := 0; i < 100; i++ {
		picked, err := pickCandidates(pool, 10)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int64]bool{}
		for _, p := range picked {
			if seen[p.UserID] {
				t.Fatal("duplicate winner")
			}
			seen[p.UserID] = true
		}
		if len(picked) != 3 {
			t.Fatal("wrong number of winners")
		}
	}
	if _, err := pickCandidates([]Candidate{{UserID: 1, Weight: 0}}, 1); err == nil {
		t.Fatal("zero weight accepted")
	}
}

func TestTenDayLimitAndPartnerEntryUniqueness(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	now := testTime(t, s.Config, "2026-11-01 12:00")
	event(t, s, "start", 1, "start", "", now)
	err := s.Store.Transaction(ctx, func(tx *sql.Tx) error {
		for _, d := range s.Config.Days {
			if err := addActivity(tx, 1, d, now); err != nil {
				return err
			}
		}
		extra := s.Config.Days[0]
		extra.Date = "2026-11-11"
		if err := addActivity(tx, 1, extra, now); err != nil {
			return err
		}
		if _, err := register(tx, 1, s.Config.Days[0], now); err != nil {
			return err
		}
		next := s.Config.Days[1]
		next.Partner = s.Config.Days[0].Partner
		created, err := register(tx, 1, next, now)
		if created {
			t.Error("same partner accepted second entry")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM activity") != 10 || scalar(t, s, "SELECT COUNT(*) FROM entries") != 1 {
		t.Fatal("activity or partner limit failed")
	}
}

func TestExpiredClaimAndPermanentDeliveryFailure(t *testing.T) {
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
	p.sendErr = &APIError{Code: 403}
	if _, err := s.DeliverOne(ctx, now); err == nil {
		t.Fatal("expected permanent delivery failure")
	}
	if scalar(t, s, "SELECT COUNT(*) FROM outbox WHERE failed_at IS NOT NULL") != 1 {
		t.Fatal("failure not recorded")
	}
	if err := s.Store.ExpirePrizes(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='expired'") != 1 || scalar(t, s, "SELECT COUNT(*) FROM outbox") != 2 {
		t.Fatal("unreachable winner not replaced")
	}
	p.sendErr = nil
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	var id, user int64
	if err := s.Store.DB.QueryRow("SELECT id,user_id FROM winners WHERE status='notified'").Scan(&id, &user); err != nil {
		t.Fatal(err)
	}
	event(t, s, "claim", user, fmt.Sprintf("claim:%d", id), "", now.Add(24*time.Hour))
	if scalar(t, s, "SELECT COUNT(*) FROM users WHERE pending_claim IS NOT NULL") != 0 {
		t.Fatal("expired claim accepted")
	}
}

func TestWebhookPersistenceAndCallbackAcknowledgement(t *testing.T) {
	s, _ := newTestService(t)
	answered := 0
	telegramAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest/answerCallbackQuery" {
			t.Error("unexpected API method")
		}
		answered++
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer telegramAPI.Close()
	telegram := NewTelegram("test")
	telegram.BaseURL = telegramAPI.URL
	h := (&HTTP{Service: s, Telegram: telegram, WebhookSecret: "secret", AdminToken: "admin"}).Handler()
	payload := `{"update_id":10,"callback_query":{"id":"cb","from":{"id":42},"data":"promo","message":{"chat":{"id":42,"type":"private"}}}}`
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/webhook", strings.NewReader(payload))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if scalar(t, s, "SELECT COUNT(*) FROM inbox") != 1 || answered != 2 {
		t.Fatal("webhook deduplication or acknowledgement failed")
	}
}

func TestExportHidesUnconfirmedNameAndEscapesFormulas(t *testing.T) {
	s, _ := newTestService(t)
	now := testTime(t, s.Config, "2026-11-02 00:00")
	ctx := context.Background()
	seedParticipants(t, s, 3, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("UPDATE winners SET full_name='Иван Иванов',consent_text='draft'"); err != nil {
		t.Fatal(err)
	}
	h := (&HTTP{Service: s, AdminToken: "admin"}).Handler()
	r := httptest.NewRequest("GET", "/admin/draws/2026-11-01/export.csv", nil)
	r.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Иван") || strings.Contains(w.Body.String(), "draft") {
		t.Fatal("unconfirmed data exported")
	}
	if csvSafe("=SUM(A1)") != "'=SUM(A1)" || csvSafe("Иван") != "Иван" {
		t.Fatal("CSV formula protection failed")
	}
}

func TestConsentKeepsShownTextAndCancelRejectsOldButton(t *testing.T) {
	s, _ := newTestService(t)
	now := testTime(t, s.Config, "2026-11-02 00:00")
	ctx := context.Background()
	seedParticipants(t, s, 3, now)
	if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Approve(ctx, s.Config, "2026-11-01", "admin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverOne(ctx, now); err != nil {
		t.Fatal(err)
	}
	var id, user int64
	if err := s.Store.DB.QueryRow("SELECT id,user_id FROM winners WHERE status='notified'").Scan(&id, &user); err != nil {
		t.Fatal(err)
	}
	event(t, s, "1", user, fmt.Sprintf("claim:%d", id), "", now)
	event(t, s, "2", user, "text", "Иван Иванов", now)
	event(t, s, "3", user, "cancel", "", now)
	event(t, s, "4", user, fmt.Sprintf("consent:%d", id), "", now)
	if scalar(t, s, "SELECT COUNT(*) FROM winners WHERE status='claimed'") != 0 {
		t.Fatal("cancelled form accepted old consent")
	}
	event(t, s, "5", user, fmt.Sprintf("claim:%d", id), "", now)
	event(t, s, "6", user, "text", "Иван Иванов", now)
	shown := s.Config.Consent
	s.Config.Consent = "New consent after restart"
	event(t, s, "7", user, fmt.Sprintf("consent:%d", id), "", now)
	var saved string
	if err := s.Store.DB.QueryRow("SELECT consent_text FROM winners WHERE id=?", id).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != shown {
		t.Fatal("stored consent differs from shown text")
	}
}
