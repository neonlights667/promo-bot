package bot

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type HTTP struct {
	Service       *Service
	Telegram      *Telegram
	WebhookSecret string
	AdminToken    string
}

func (h *HTTP) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Service.Store.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /webhook", h.webhook)
	admin := http.NewServeMux()
	admin.HandleFunc("GET /admin/stats", h.stats)
	admin.HandleFunc("POST /admin/promos/{day}", h.importPromos)
	admin.HandleFunc("GET /admin/draws", h.draws)
	admin.HandleFunc("GET /admin/draws/{draw}/export.csv", h.exportDraw)
	admin.HandleFunc("POST /admin/draws/{draw}/approve", h.approve)
	admin.HandleFunc("POST /admin/broadcasts", h.broadcast)
	admin.HandleFunc("GET /admin/messages/failed", h.failedMessages)
	admin.HandleFunc("POST /admin/messages/{id}/retry", h.retryMessage)
	mux.Handle("/admin/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.AdminToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		admin.ServeHTTP(w, r.WithContext(ctx))
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "error", err)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("admin request failed", "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (h *HTTP) webhook(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(h.WebhookSecret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var update TelegramUpdate
	if err := readJSON(w, r, &update); err != nil || update.UpdateID == nil {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	e, ok := update.Event()
	if ok {
		if err := h.Service.Receive(r.Context(), e, time.Now()); err != nil {
			serverError(w, err)
			return
		}
		if update.Callback != nil {
			if err := h.Telegram.AnswerCallback(r.Context(), update.Callback.ID); err != nil {
				slog.Warn("answer callback failed", "error", err)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (h *HTTP) importPromos(w http.ResponseWriter, r *http.Request) {
	day := r.PathValue("day")
	if _, ok := h.Service.Config.Day(day); !ok {
		http.Error(w, "unknown day", http.StatusNotFound)
		return
	}
	var body struct {
		Codes []string `json:"codes"`
	}
	if err := readJSON(w, r, &body); err != nil || len(body.Codes) == 0 || len(body.Codes) > 10000 {
		http.Error(w, "expected 1-10000 codes", http.StatusBadRequest)
		return
	}
	n, err := h.Service.Store.ImportPromos(r.Context(), day, body.Codes, time.Now())
	if err != nil {
		http.Error(w, "import rejected: check codes and their assigned days", http.StatusBadRequest)
		slog.Warn("promo import rejected", "error", err)
		return
	}
	writeJSON(w, map[string]int{"imported": n})
}

func (h *HTTP) approve(w http.ResponseWriter, r *http.Request) {
	err := h.Service.Store.Approve(r.Context(), h.Service.Config, r.PathValue("draw"), "admin", time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "draw not found", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "approved"})
}

func (h *HTTP) draws(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Service.Store.DB.QueryContext(r.Context(), `SELECT d.id,d.created_at,d.approved_at,
	 (SELECT COUNT(*) FROM draw_entries e WHERE e.draw_id=d.id),
	 (SELECT COUNT(*) FROM winners v WHERE v.draw_id=d.id AND v.status IN ('pending','notified','claimed')) FROM draws d ORDER BY d.id`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type draw struct {
		ID           string `json:"id"`
		CreatedAt    string `json:"created_at"`
		ApprovedAt   string `json:"approved_at,omitempty"`
		Participants int    `json:"participants"`
		Winners      int    `json:"winners"`
	}
	result := make([]draw, 0)
	for rows.Next() {
		var d draw
		var created int64
		var approved sql.NullInt64
		if err = rows.Scan(&d.ID, &created, &approved, &d.Participants, &d.Winners); err != nil {
			serverError(w, err)
			return
		}
		d.CreatedAt = time.Unix(created, 0).UTC().Format(time.RFC3339)
		if approved.Valid {
			d.ApprovedAt = time.Unix(approved.Int64, 0).UTC().Format(time.RFC3339)
		}
		result = append(result, d)
	}
	if err = rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, result)
}

func (h *HTTP) stats(w http.ResponseWriter, r *http.Request) {
	db := h.Service.Store.DB
	ctx := r.Context()
	type dayStats struct {
		Day          string `json:"day"`
		Partner      string `json:"partner"`
		Requests     int    `json:"promo_requests"`
		Issued       int    `json:"promos_issued"`
		Remaining    int    `json:"promos_remaining"`
		Participants int    `json:"participants"`
	}
	type newUsers struct {
		Day   string `json:"day"`
		Count int    `json:"count"`
	}
	result := struct {
		Users           int        `json:"users"`
		NewUsers        []newUsers `json:"new_users"`
		Days            []dayStats `json:"days"`
		PendingMessages int        `json:"pending_messages"`
		FailedMessages  int        `json:"failed_messages"`
		PendingEvents   int        `json:"pending_events"`
	}{NewUsers: []newUsers{}, Days: []dayStats{}}
	err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM users),
	 (SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND failed_at IS NULL),
	 (SELECT COUNT(*) FROM outbox WHERE failed_at IS NOT NULL),
	 (SELECT COUNT(*) FROM inbox WHERE processed_at IS NULL)`).Scan(&result.Users, &result.PendingMessages, &result.FailedMessages, &result.PendingEvents)
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err := db.QueryContext(ctx, "SELECT start_day,COUNT(*) FROM users GROUP BY start_day ORDER BY start_day")
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var n newUsers
		if err = rows.Scan(&n.Day, &n.Count); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		result.NewUsers = append(result.NewUsers, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		serverError(w, err)
		return
	}
	for _, d := range h.Service.Config.Days {
		ds := dayStats{Day: d.Date, Partner: d.Partner}
		err = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM promo_requests WHERE day=?),
		 (SELECT COUNT(*) FROM promos WHERE day=? AND user_id IS NOT NULL),
		 (SELECT COUNT(*) FROM promos WHERE day=? AND user_id IS NULL),
		 (SELECT COUNT(*) FROM entry_confirmations WHERE day=?)`, d.Date, d.Date, d.Date, d.Date).Scan(&ds.Requests, &ds.Issued, &ds.Remaining, &ds.Participants)
		if err != nil {
			serverError(w, err)
			return
		}
		result.Days = append(result.Days, ds)
	}
	writeJSON(w, result)
}

func csvSafe(value string) string {
	if value != "" && strings.ContainsAny(value[:1], "=+-@\t\r\n") {
		return "'" + value
	}
	return value
}

func (h *HTTP) exportDraw(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("draw")
	var created int64
	var approved sql.NullInt64
	err := h.Service.Store.DB.QueryRowContext(r.Context(), "SELECT created_at,approved_at FROM draws WHERE id=?", id).Scan(&created, &approved)
	if err == sql.ErrNoRows {
		http.Error(w, "draw not found", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err := h.Service.Store.DB.QueryContext(r.Context(), `SELECT e.user_id,e.weight,COALESCE(w.rank,0),COALESCE(w.prize,''),COALESCE(w.status,'participant'),
	 CASE WHEN w.consent_at IS NOT NULL THEN w.full_name ELSE '' END,COALESCE(w.consent_at,0),CASE WHEN w.consent_at IS NOT NULL THEN w.consent_text ELSE '' END,COALESCE(p.registered_at,0)
	 FROM draw_entries e LEFT JOIN winners w ON w.draw_id=e.draw_id AND w.user_id=e.user_id
	 LEFT JOIN entries p ON p.day=e.draw_id AND p.user_id=e.user_id WHERE e.draw_id=? ORDER BY e.user_id`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="draw.csv"`)
	writer := csv.NewWriter(w)
	if err = writer.Write([]string{"draw", "drawn_at", "approved_at", "user_id", "weight", "rank", "prize", "status", "full_name", "consent_at", "consent_text", "registered_at"}); err != nil {
		slog.Error("export header", "error", err)
		return
	}
	stamp := func(v int64) string {
		if v == 0 {
			return ""
		}
		return time.Unix(v, 0).UTC().Format(time.RFC3339)
	}
	for rows.Next() {
		var user, consent, registered int64
		var weight, rank int
		var prize, status, name, consentText string
		if err = rows.Scan(&user, &weight, &rank, &prize, &status, &name, &consent, &consentText, &registered); err != nil {
			slog.Error("export row", "error", err)
			return
		}
		record := []string{id, stamp(created), stamp(approved.Int64), strconv.FormatInt(user, 10), strconv.Itoa(weight), strconv.Itoa(rank), prize, status, name, stamp(consent), consentText, stamp(registered)}
		for i := range record {
			record[i] = csvSafe(record[i])
		}
		if err = writer.Write(record); err != nil {
			slog.Error("export write", "error", err)
			return
		}
	}
	if err = rows.Err(); err != nil {
		slog.Error("export query", "error", err)
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		slog.Error("export flush", "error", err)
	}
}

func (h *HTTP) broadcast(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := readJSON(w, r, &body); err != nil || len(body.ID) < 1 || len(body.ID) > 100 || len([]rune(body.Text)) < 1 || len([]rune(body.Text)) > 3500 {
		http.Error(w, "provide id and text (1-3500 characters)", http.StatusBadRequest)
		return
	}
	count := int64(0)
	err := h.Service.Store.Transaction(r.Context(), func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRow("SELECT text FROM broadcasts WHERE id=?", body.ID).Scan(&existing)
		if err == nil {
			if existing != body.Text {
				return fmt.Errorf("broadcast id is already used")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		now := time.Now().Unix()
		if _, err = tx.Exec("INSERT INTO broadcasts(id,text,created_at) VALUES(?,?,?)", body.ID, body.Text, now); err != nil {
			return err
		}
		data, err := json.Marshal(Message{Text: body.Text, Buttons: menu()})
		if err != nil {
			return err
		}
		res, err := tx.Exec("INSERT INTO outbox(user_id,payload,created_at,broadcast) SELECT id,?,?,1 FROM users WHERE enabled=1", data, now)
		if err != nil {
			return err
		}
		count, err = res.RowsAffected()
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO audit(actor,action,target,created_at) VALUES('admin','broadcast',?,?)", body.ID, now)
		return err
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]int64{"queued": count})
}
