package bot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"time"
)

type Candidate struct {
	UserID int64
	Weight int
}

func pickCandidates(pool []Candidate, count int) ([]Candidate, error) {
	pool = append([]Candidate(nil), pool...)
	chosen := make([]Candidate, 0, count)
	for len(chosen) < count && len(pool) > 0 {
		total := 0
		for _, p := range pool {
			if p.Weight < 1 || p.Weight > 10 {
				return nil, fmt.Errorf("invalid weight")
			}
			total += p.Weight
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(total)))
		if err != nil {
			return nil, err
		}
		position := int(n.Int64())
		for i, p := range pool {
			position -= p.Weight
			if position < 0 {
				chosen = append(chosen, p)
				pool = append(pool[:i], pool[i+1:]...)
				break
			}
		}
	}
	return chosen, nil
}

func (s *Store) PrepareDraws(ctx context.Context, c Config, now time.Time) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		for _, d := range c.Days {
			date, _ := time.ParseInLocation(time.DateOnly, d.Date, c.Location)
			if now.Before(date.AddDate(0, 0, 1)) {
				continue
			}
			var pending int
			if err := tx.QueryRow("SELECT COUNT(*) FROM inbox WHERE processed_at IS NULL AND created_at>=? AND created_at<?", date.Unix(), date.AddDate(0, 0, 1).Unix()).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			if err := tx.QueryRow(`SELECT COUNT(*) FROM outbox o
			 WHERE o.sent_at IS NULL AND o.failed_at IS NULL
			 AND json_extract(CAST(o.payload AS TEXT),'$.registration_day')=?
			 AND NOT EXISTS(SELECT 1 FROM entry_confirmations c WHERE c.day=? AND c.user_id=o.user_id)`, d.Date, d.Date).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			if err := prepareDraw(tx, d.Date, d.Prizes, c.ReserveCount, now, false); err != nil {
				return err
			}
		}
		last, _ := time.ParseInLocation(time.DateOnly, c.Days[len(c.Days)-1].Date, c.Location)
		if !now.Before(last.AddDate(0, 0, 1)) {
			first, _ := time.ParseInLocation(time.DateOnly, c.Days[0].Date, c.Location)
			var pending int
			if err := tx.QueryRow("SELECT COUNT(*) FROM inbox WHERE processed_at IS NULL AND created_at>=? AND created_at<?", first.Unix(), last.AddDate(0, 0, 1).Unix()).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				return nil
			}
			prizes := make([]string, 5)
			for i := range prizes {
				prizes[i] = c.GrandPrize
			}
			return prepareDraw(tx, "final", prizes, c.ReserveCount, now, true)
		}
		return nil
	})
}

func prepareDraw(tx *sql.Tx, id string, prizes []string, reserve int, now time.Time, final bool) error {
	r, err := tx.Exec("INSERT INTO draws(id,created_at) VALUES(?,?) ON CONFLICT DO NOTHING", id, now.Unix())
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	query := `SELECT e.user_id,1 FROM entries e WHERE e.day=?
	 AND EXISTS(SELECT 1 FROM entry_confirmations c WHERE c.day=e.day AND c.user_id=e.user_id) ORDER BY e.user_id`
	args := []any{id}
	if final {
		query = "SELECT user_id,COUNT(*) FROM activity GROUP BY user_id ORDER BY user_id"
		args = nil
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	var pool []Candidate
	for rows.Next() {
		var p Candidate
		if err = rows.Scan(&p.UserID, &p.Weight); err != nil {
			rows.Close()
			return err
		}
		pool = append(pool, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range pool {
		if _, err = tx.Exec("INSERT INTO draw_entries(draw_id,user_id,weight) VALUES(?,?,?)", id, p.UserID, p.Weight); err != nil {
			return err
		}
	}
	chosen, err := pickCandidates(pool, len(prizes)+reserve)
	if err != nil {
		return err
	}
	for i, p := range chosen {
		status, prize := "reserve", ""
		if i < len(prizes) {
			status = "pending"
			prize = prizes[i]
		}
		if _, err = tx.Exec("INSERT INTO winners(draw_id,user_id,rank,prize,status) VALUES(?,?,?,?,?)", id, p.UserID, i+1, prize, status); err != nil {
			return err
		}
	}
	return nil
}

func prizeMessage(c Config, draw, prize string, id int64) Message {
	text := "Ты в числе хедлайнеров дня!\n\nТы выиграл " + prize + "."
	if d, ok := c.Day(draw); ok {
		text += "\nПартнер: " + d.Partner + ".\n" + d.ClaimInstruction
	}
	if draw == "final" {
		text = "Твой фестиваль выходит на главную сцену!\n\nТы выиграл " + prize + "."
	}
	text += fmt.Sprintf("\n\nНажми кнопку и оставь имя и фамилию. Подтвердить получение нужно за %d ч. после этого уведомления.", c.ClaimHours)
	return Message{Text: text, Buttons: [][]Button{{callback("Получить приз", fmt.Sprintf("claim:%d", id))}}}
}

func (s *Store) Approve(ctx context.Context, c Config, draw, actor string, now time.Time) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.Exec("UPDATE draws SET approved_at=?,approved_by=? WHERE id=? AND approved_at IS NULL", now.Unix(), actor, draw)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			var exists int
			if err = tx.QueryRow("SELECT 1 FROM draws WHERE id=?", draw).Scan(&exists); err != nil {
				return err
			}
			return nil
		}
		rows, err := tx.Query("SELECT id,user_id,prize FROM winners WHERE draw_id=? AND status='pending' ORDER BY rank", draw)
		if err != nil {
			return err
		}
		type winner struct {
			id, user int64
			prize    string
		}
		var winners []winner
		for rows.Next() {
			var w winner
			if err = rows.Scan(&w.id, &w.user, &w.prize); err != nil {
				rows.Close()
				return err
			}
			winners = append(winners, w)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, w := range winners {
			if err = queue(tx, w.user, prizeMessage(c, draw, w.prize, w.id), w.id, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec("INSERT INTO audit(actor,action,target,created_at) VALUES(?,'approve',?,?)", actor, draw, now.Unix())
		return err
	})
}

func (s *Store) ExpirePrizes(ctx context.Context, c Config, now time.Time) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT w.id,w.draw_id,w.prize FROM winners w WHERE w.status='notified' AND w.deadline<=?
		 AND NOT EXISTS(SELECT 1 FROM inbox i WHERE i.user_id=w.user_id AND i.processed_at IS NULL AND i.created_at<w.deadline)
		 ORDER BY w.id`, now.Unix())
		if err != nil {
			return err
		}
		type expired struct {
			id          int64
			draw, prize string
		}
		var expiredPrizes []expired
		for rows.Next() {
			var w expired
			if err = rows.Scan(&w.id, &w.draw, &w.prize); err != nil {
				rows.Close()
				return err
			}
			expiredPrizes = append(expiredPrizes, w)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, w := range expiredPrizes {
			if _, err = tx.Exec("UPDATE winners SET status='expired' WHERE id=?", w.id); err != nil {
				return err
			}
			var id, user int64
			err = tx.QueryRow("SELECT id,user_id FROM winners WHERE draw_id=? AND status='reserve' ORDER BY rank LIMIT 1", w.draw).Scan(&id, &user)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE winners SET status='pending',prize=? WHERE id=?", w.prize, id); err != nil {
				return err
			}
			if err = queue(tx, user, prizeMessage(c, w.draw, w.prize, id), id, now); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO audit(actor,action,target,created_at) VALUES('scheduler','promote',?,?)", fmt.Sprint(id), now.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}
