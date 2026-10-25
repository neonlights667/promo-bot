package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

func (s *Service) DeliverOne(ctx context.Context, now time.Time) (bool, error) {
	var id, user int64
	var winner sql.NullInt64
	var data []byte
	var attempts, enabled, broadcast int
	err := s.Store.DB.QueryRowContext(ctx, `SELECT o.id,o.user_id,o.payload,o.winner_id,o.attempts,u.enabled,o.broadcast
	 FROM outbox o JOIN users u ON u.id=o.user_id
	 WHERE o.sent_at IS NULL AND o.failed_at IS NULL AND o.next_attempt<=?
	 AND NOT EXISTS(SELECT 1 FROM outbox earlier WHERE earlier.user_id=o.user_id AND earlier.broadcast=o.broadcast AND earlier.id<o.id AND earlier.sent_at IS NULL AND earlier.failed_at IS NULL)
	 AND ((o.broadcast=1 AND u.enabled=0) OR NOT EXISTS(SELECT 1 FROM outbox recent WHERE recent.user_id=o.user_id AND recent.sent_at>?))
	 ORDER BY o.broadcast,o.id LIMIT 1`, now.Unix(), now.Unix()-2).Scan(&id, &user, &data, &winner, &attempts, &enabled, &broadcast)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if enabled == 0 && broadcast == 1 {
		_, err = s.Store.DB.ExecContext(ctx, "UPDATE outbox SET failed_at=? WHERE id=?", now.Unix(), id)
		return true, err
	}
	var m Message
	if err = json.Unmarshal(data, &m); err != nil {
		return false, err
	}
	started := time.Now()
	err = s.Platform.Send(ctx, user, m)
	now = now.Add(time.Since(started))
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && (api.Code == 400 || api.Code == 403) {
			if m.ImageURL != "" && api.Code == 400 {
				m.ImageURL = ""
				replacement, encodeErr := json.Marshal(m)
				if encodeErr != nil {
					return true, encodeErr
				}
				_, updateErr := s.Store.DB.ExecContext(ctx, "UPDATE outbox SET payload=?,attempts=attempts+1 WHERE id=?", replacement, id)
				return true, updateErr
			}
			storeErr := s.Store.Transaction(ctx, func(tx *sql.Tx) error {
				if _, e := tx.Exec("UPDATE outbox SET failed_at=?,attempts=attempts+1 WHERE id=?", now.Unix(), id); e != nil {
					return e
				}
				if api.Code == 403 {
					if _, e := tx.Exec("UPDATE users SET enabled=0 WHERE id=?", user); e != nil {
						return e
					}
				}
				if winner.Valid && api.Code == 403 {
					_, e := tx.Exec("UPDATE winners SET status='notified',deadline=? WHERE id=? AND status='pending'", now.Unix(), winner.Int64)
					return e
				}
				return nil
			})
			if storeErr != nil {
				return true, storeErr
			}
			return true, fmt.Errorf("outbox %d permanently failed: %w", id, err)
		}
		delay := min(3600, 1<<min(attempts+1, 11))
		if api != nil && api.RetryAfter > delay {
			delay = api.RetryAfter
		}
		_, storeErr := s.Store.DB.ExecContext(ctx, "UPDATE outbox SET attempts=attempts+1,next_attempt=? WHERE id=?", now.Add(time.Duration(delay)*time.Second).Unix(), id)
		if storeErr != nil {
			return true, storeErr
		}
		return true, fmt.Errorf("outbox %d retry: %w", id, err)
	}
	err = s.Store.Transaction(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec("UPDATE outbox SET sent_at=?,payload='{}' WHERE id=?", now.Unix(), id); e != nil {
			return e
		}
		if m.RegistrationDay != "" {
			if _, e := tx.Exec("INSERT INTO entry_confirmations(day,user_id,confirmed_at) VALUES(?,?,?) ON CONFLICT DO NOTHING", m.RegistrationDay, user, now.Unix()); e != nil {
				return e
			}
		}
		if winner.Valid {
			_, e := tx.Exec("UPDATE winners SET status='notified',deadline=? WHERE id=? AND status='pending'", now.Add(time.Duration(s.Config.ClaimHours)*time.Hour).Unix(), winner.Int64)
			return e
		}
		return nil
	})
	return true, err
}

func (s *Service) Run(ctx context.Context) {
	var workers sync.WaitGroup
	start := func(interval time.Duration, run func(time.Time)) {
		workers.Go(func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					run(time.Now())
				}
			}
		})
	}
	start(50*time.Millisecond, func(now time.Time) {
		if _, err := s.ProcessOne(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("process event", "error", err)
		}
	})
	start(50*time.Millisecond, func(now time.Time) {
		if _, err := s.DeliverOne(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("deliver message", "error", err)
		}
	})
	start(10*time.Second, func(now time.Time) {
		if err := s.Store.PrepareDraws(ctx, s.Config, now); err != nil && ctx.Err() == nil {
			slog.Error("prepare draws", "error", err)
		}
		if err := s.Store.ExpirePrizes(ctx, s.Config, now); err != nil && ctx.Err() == nil {
			slog.Error("expire prizes", "error", err)
		}
	})
	workers.Wait()
}
