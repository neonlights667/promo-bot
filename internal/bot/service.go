package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Service struct {
	Store    *Store
	Config   Config
	Platform Platform
}

func (s *Service) Receive(ctx context.Context, e Event, now time.Time) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.Store.DB.ExecContext(ctx, "INSERT INTO inbox(id,user_id,payload,created_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", e.ID, e.UserID, data, now.Unix())
	return err
}

func (s *Service) ProcessOne(ctx context.Context, now time.Time) (bool, error) {
	var data []byte
	var id string
	var received int64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT i.id,i.payload,i.created_at FROM inbox i
	 WHERE i.processed_at IS NULL AND i.next_attempt<=?
	 AND NOT EXISTS (SELECT 1 FROM inbox older WHERE older.user_id=i.user_id AND older.processed_at IS NULL AND older.rowid<i.rowid)
	 ORDER BY i.rowid LIMIT 1`, now.Unix()).Scan(&id, &data, &received)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var e Event
	if err = json.Unmarshal(data, &e); err != nil {
		return false, err
	}
	member, checkFailed := false, false
	eventTime := time.Unix(received, 0)
	d, active := s.Config.Current(eventTime)
	if e.Action == "check:"+d.Date && active {
		member, err = s.Platform.IsMember(ctx, d.ChannelID, e.UserID)
		checkFailed = err != nil
	}
	err = s.Store.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO users(id,started_at,start_day) VALUES(?,?,?) ON CONFLICT DO NOTHING`, e.UserID, received, eventTime.In(s.Config.Location).Format(time.DateOnly)); err != nil {
			return err
		}
		message, err := s.respond(tx, e, eventTime, member, checkFailed)
		if err != nil {
			return err
		}
		if err = queue(tx, e.UserID, message, nil, now); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE inbox SET processed_at=?,payload='{}' WHERE id=?", now.Unix(), id)
		return err
	})
	if err != nil {
		_, retryErr := s.Store.DB.ExecContext(ctx, "UPDATE inbox SET attempts=attempts+1,next_attempt=? WHERE id=?", now.Add(5*time.Second).Unix(), id)
		if retryErr != nil {
			return true, fmt.Errorf("process: %v; retry: %w", err, retryErr)
		}
	}
	return true, err
}

func (s *Service) respond(tx *sql.Tx, e Event, now time.Time, member, checkFailed bool) (Message, error) {
	m := Message{Buttons: menu()}
	d, active := s.Config.Current(now)
	switch {
	case e.Action == "start":
		_, err := tx.Exec("UPDATE users SET enabled=1,pending_claim=NULL WHERE id=?", e.UserID)
		m.Text = "Добро пожаловать в Бот розыгрышей"
		m.ImageURL = s.Config.BannerURL
		return m, err
	case e.Action == "stop":
		_, err := tx.Exec("UPDATE users SET enabled=0,pending_claim=NULL WHERE id=?", e.UserID)
		m.Text = "Рассылка отключена. Чтобы включить ее снова, отправь /start."
		return m, err
	case e.Action == "rules":
		m.Text = s.Config.Rules
		return m, nil
	case e.Action == "help":
		m.Text = "Каждый день участвуй в розыгрыше партнера или получай промокод. Один активный день дает один шанс на главный приз, максимум 10. Для ежедневного розыгрыша подпишись на партнера и проверь подписку. Промокод для участия не нужен.\n\n/stop отключает рассылку. /cancel отменяет ввод данных для приза."
		return m, nil
	case e.Action == "chances":
		return s.chances(tx, e.UserID, now)
	case e.Action == "cancel":
		_, err := tx.Exec("UPDATE users SET pending_claim=NULL WHERE id=?", e.UserID)
		m.Text = "Ввод данных отменен. Получить приз можно по кнопке в уведомлении."
		return m, err
	case strings.HasPrefix(e.Action, "claim:"):
		return s.beginClaim(tx, e, now)
	case strings.HasPrefix(e.Action, "consent:"):
		return s.confirmClaim(tx, e, now)
	case e.Action == "promo" || e.Action == "enter" || strings.HasPrefix(e.Action, "check:"):
		if !active {
			m.Text = "Сегодня нет партнерского розыгрыша. Расписание и условия доступны в правилах акции."
			return m, nil
		}
		if e.Action == "promo" {
			code, err := givePromo(tx, e.UserID, d, now)
			if err != nil {
				return m, err
			}
			if code == "" {
				m.Text = "Промокоды от " + d.Partner + " уже закончились. Но ты все еще можешь подписаться на партнера и принять участие в розыгрыше призов дня."
			} else {
				m.Text = fmt.Sprintf("Твой фестивальный бонус от %s готов!\n\nПромокод: %s\n\nИспользуй его до %s: %s.\n\nТеперь можешь принять участие в розыгрыше призов дня.", d.Partner, code, d.PromoUntil, d.PromoUsage)
			}
			return m, nil
		}
		var registeredDay string
		err := tx.QueryRow("SELECT day FROM entries WHERE user_id=? AND (day=? OR partner=?)", e.UserID, d.Date, d.Partner).Scan(&registeredDay)
		if err == nil {
			m.Text = "Ты уже участвуешь в розыгрыше этого партнера. Результаты сообщим после подведения итогов."
			m.RegistrationDay = registeredDay
			return m, nil
		}
		if err != sql.ErrNoRows {
			return m, err
		}
		m.Buttons = subscriptionButtons(d)
		if e.Action == "enter" {
			m.Text = "Чтобы попасть в розыгрыш призов от " + d.Partner + ", подпишись на аккаунт партнера."
			return m, nil
		}
		if e.Action != "check:"+d.Date {
			m.Text = "Партнер сменился. Подпишись на сегодняшнего партнера и проверь подписку еще раз."
			return m, nil
		}
		if checkFailed {
			m.Text = "Не удалось проверить подписку. Попробуй еще раз чуть позже."
			return m, nil
		}
		if !member {
			m.Text = "Пока не вижу подписку. Подпишись на аккаунт " + d.Partner + ", вернись в бот и попробуй еще раз."
			return m, nil
		}
		_, err = register(tx, e.UserID, d, now)
		if err != nil {
			return m, err
		}
		var count int
		if err = tx.QueryRow("SELECT COUNT(*) FROM activity WHERE user_id=?", e.UserID).Scan(&count); err != nil {
			return m, err
		}
		m.Text = fmt.Sprintf("Ты в лайнапе!\n\nЗаявка на розыгрыш призов от %s зарегистрирована.\nНакоплено шансов: %d из 10.\n\nВозвращайся завтра, на сцену выйдет новый партнер.", d.Partner, count)
		m.Buttons = menu()
		m.RegistrationDay = d.Date
		return m, nil
	case e.Action == "text":
		return s.saveName(tx, e, now)
	default:
		m.Text = "Выбери действие в меню."
		return m, nil
	}
}

func (s *Service) chances(tx *sql.Tx, user int64, now time.Time) (Message, error) {
	rows, err := tx.Query("SELECT day,partner FROM activity WHERE user_id=? ORDER BY day", user)
	if err != nil {
		return Message{}, err
	}
	var days []string
	for rows.Next() {
		var day, partner string
		if err = rows.Scan(&day, &partner); err != nil {
			rows.Close()
			return Message{}, err
		}
		days = append(days, day+": "+partner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Message{}, err
	}
	remaining := 0
	for _, d := range s.Config.Days {
		if d.Date > now.In(s.Config.Location).Format(time.DateOnly) {
			remaining++
		}
	}
	text := fmt.Sprintf("Твой фестивальный пропуск\nНакоплено шансов: %d из 10\n\nПройденные дни и партнеры:\n%s\n\nОсталось дней после сегодняшнего: %d\nГлавный приз: %s.\nПять победителей получат по два билета.\n\nЧем больше активных дней, тем выше вероятность выиграть главный приз.", len(days), strings.Join(days, "\n"), remaining, s.Config.GrandPrize)
	return Message{Text: text, Buttons: menu()}, nil
}

func (s *Service) beginClaim(tx *sql.Tx, e Event, now time.Time) (Message, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(e.Action, "claim:"), 10, 64)
	if err != nil {
		return Message{Text: "Не удалось найти приз."}, nil
	}
	var exists int
	err = tx.QueryRow("SELECT 1 FROM winners WHERE id=? AND user_id=? AND status='notified' AND deadline>?", id, e.UserID, now.Unix()).Scan(&exists)
	if err == sql.ErrNoRows {
		return Message{Text: "Приз уже подтвержден или срок получения истек."}, nil
	}
	if err != nil {
		return Message{}, err
	}
	if _, err = tx.Exec("UPDATE winners SET full_name=NULL,consent_text=NULL WHERE id=?", id); err != nil {
		return Message{}, err
	}
	_, err = tx.Exec("UPDATE users SET pending_claim=? WHERE id=?", id, e.UserID)
	return Message{Text: "Напиши имя и фамилию одним сообщением. /cancel отменяет ввод."}, err
}

func validName(name string) bool {
	parts := strings.Fields(name)
	if len(parts) < 2 || len(parts) > 5 || len([]rune(name)) > 150 {
		return false
	}
	for _, p := range parts {
		letters := 0
		for _, r := range p {
			if unicode.IsLetter(r) {
				letters++
			} else if r != '-' && r != '\'' {
				return false
			}
		}
		if letters == 0 {
			return false
		}
	}
	return true
}

func (s *Service) saveName(tx *sql.Tx, e Event, now time.Time) (Message, error) {
	var id sql.NullInt64
	if err := tx.QueryRow("SELECT pending_claim FROM users WHERE id=?", e.UserID).Scan(&id); err != nil {
		return Message{}, err
	}
	if !id.Valid {
		return Message{Text: "Выбери действие в меню.", Buttons: menu()}, nil
	}
	name := strings.Join(strings.Fields(e.Text), " ")
	if !validName(name) {
		return Message{Text: "Нужны имя и фамилия, например: Иван Иванов. /cancel отменяет ввод."}, nil
	}
	r, err := tx.Exec("UPDATE winners SET full_name=?,consent_text=? WHERE id=? AND user_id=? AND status='notified' AND deadline>?", name, s.Config.Consent, id.Int64, e.UserID, now.Unix())
	if err != nil {
		return Message{}, err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return Message{Text: "Срок получения этого приза истек."}, nil
	}
	return Message{Text: "Данные: " + name + "\n\n" + s.Config.Consent, Buttons: [][]Button{{callback("Согласен, получить приз", fmt.Sprintf("consent:%d", id.Int64))}, {callback("Отмена", "cancel")}}}, nil
}

func (s *Service) confirmClaim(tx *sql.Tx, e Event, now time.Time) (Message, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(e.Action, "consent:"), 10, 64)
	if err != nil {
		return Message{Text: "Не удалось найти приз."}, nil
	}
	r, err := tx.Exec(`UPDATE winners SET status='claimed',consent_at=? WHERE id=? AND user_id=? AND status='notified' AND deadline>? AND full_name IS NOT NULL AND consent_text IS NOT NULL
	 AND EXISTS(SELECT 1 FROM users WHERE id=? AND pending_claim=?)`, now.Unix(), id, e.UserID, now.Unix(), e.UserID, id)
	if err != nil {
		return Message{}, err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return Message{Text: "Сначала нажми кнопку получения приза и введи данные. Срок получения должен быть действующим."}, nil
	}
	_, err = tx.Exec("UPDATE users SET pending_claim=NULL WHERE id=?", e.UserID)
	return Message{Text: "Получение приза подтверждено. Организатор получил твое имя и согласие на передачу данных.", Buttons: menu()}, err
}
