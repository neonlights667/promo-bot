package bot

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"strings"
	"time"
)

//go:embed schema.sql
var schema string

type Store struct{ DB *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", schema} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{DB: db}, nil
}

func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func addActivity(tx *sql.Tx, user int64, d Day, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO activity(day,user_id,partner,created_at)
	 SELECT ?,?,?,? WHERE (SELECT COUNT(*) FROM activity WHERE user_id=?) < 10
	 ON CONFLICT(day,user_id) DO NOTHING`, d.Date, user, d.Partner, now.Unix(), user)
	return err
}

func givePromo(tx *sql.Tx, user int64, d Day, now time.Time) (string, error) {
	if _, err := tx.Exec("INSERT INTO promo_requests(user_id,day,created_at) VALUES(?,?,?)", user, d.Date, now.Unix()); err != nil {
		return "", err
	}
	var code string
	err := tx.QueryRow("SELECT code FROM promos WHERE day=? AND user_id=?", d.Date, user).Scan(&code)
	if err == nil {
		return code, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	err = tx.QueryRow("SELECT code FROM promos WHERE day=? AND user_id IS NULL ORDER BY code LIMIT 1", d.Date).Scan(&code)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec("UPDATE promos SET user_id=?,issued_at=? WHERE code=?", user, now.Unix(), code); err != nil {
		return "", err
	}
	return code, addActivity(tx, user, d, now)
}

func register(tx *sql.Tx, user int64, d Day, now time.Time) (bool, error) {
	result, err := tx.Exec("INSERT INTO entries(day,partner,user_id,registered_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", d.Date, d.Partner, user, now.Unix())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	return true, addActivity(tx, user, d, now)
}

func queue(tx *sql.Tx, user int64, message Message, winner any, now time.Time) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO outbox(user_id,payload,winner_id,created_at) VALUES(?,?,?,?)", user, data, winner, now.Unix())
	return err
}

func (s *Store) ImportPromos(ctx context.Context, day string, codes []string, now time.Time) (int, error) {
	count := 0
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		for _, code := range codes {
			code = strings.TrimSpace(code)
			if code == "" || len(code) > 128 || strings.ContainsAny(code, "\r\n\t") {
				return fmt.Errorf("invalid promo code")
			}
			var existing string
			err := tx.QueryRow("SELECT day FROM promos WHERE code=?", code).Scan(&existing)
			if err == nil {
				if existing != day {
					return fmt.Errorf("code already belongs to another day")
				}
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			if _, err = tx.Exec("INSERT INTO promos(code,day) VALUES(?,?)", code, day); err != nil {
				return err
			}
			count++
		}
		_, err := tx.Exec("INSERT INTO audit(actor,action,target,created_at) VALUES('admin','import',?,?)", day, now.Unix())
		return err
	})
	return count, err
}
