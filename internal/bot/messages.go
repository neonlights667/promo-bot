package bot

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (h *HTTP) failedMessages(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Service.Store.DB.QueryContext(r.Context(), "SELECT id,user_id,COALESCE(winner_id,0),attempts,failed_at FROM outbox WHERE failed_at IS NOT NULL ORDER BY id DESC LIMIT 100")
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type failedMessage struct {
		ID       int64 `json:"id"`
		UserID   int64 `json:"user_id"`
		WinnerID int64 `json:"winner_id,omitempty"`
		Attempts int   `json:"attempts"`
		FailedAt int64 `json:"failed_at"`
	}
	result := make([]failedMessage, 0)
	for rows.Next() {
		var m failedMessage
		if err = rows.Scan(&m.ID, &m.UserID, &m.WinnerID, &m.Attempts, &m.FailedAt); err != nil {
			serverError(w, err)
			return
		}
		result = append(result, m)
	}
	if err = rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, result)
}

func (h *HTTP) retryMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if r.ContentLength != 0 {
		if err = readJSON(w, r, &body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	}
	if len([]rune(body.Text)) > 3500 {
		http.Error(w, "text is too long", http.StatusBadRequest)
		return
	}
	conflict := errors.New("winner no longer pending")
	err = h.Service.Store.Transaction(r.Context(), func(tx *sql.Tx) error {
		var data []byte
		var winner sql.NullInt64
		if err := tx.QueryRow("SELECT payload,winner_id FROM outbox WHERE id=? AND failed_at IS NOT NULL", id).Scan(&data, &winner); err != nil {
			return err
		}
		if winner.Valid {
			var status string
			if err := tx.QueryRow("SELECT status FROM winners WHERE id=?", winner.Int64).Scan(&status); err != nil {
				return err
			}
			if status != "pending" {
				return conflict
			}
		}
		if body.Text != "" {
			var m Message
			if err := json.Unmarshal(data, &m); err != nil {
				return err
			}
			m.Text = body.Text
			m.ImageURL = ""
			var err error
			data, err = json.Marshal(m)
			if err != nil {
				return err
			}
		}
		now := time.Now().Unix()
		if _, err := tx.Exec("UPDATE outbox SET payload=?,failed_at=NULL,attempts=0,next_attempt=? WHERE id=?", data, now, id); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO audit(actor,action,target,created_at) VALUES('admin','retry_message',?,?)", fmt.Sprint(id), now)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "failed message not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, conflict) {
		http.Error(w, "winner no longer pending", http.StatusConflict)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "queued"})
}
