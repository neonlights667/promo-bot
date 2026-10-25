package bot

import "context"

type Button struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	URL     string `json:"url,omitempty"`
}

type Message struct {
	Text            string     `json:"text"`
	Buttons         [][]Button `json:"buttons,omitempty"`
	ImageURL        string     `json:"image_url,omitempty"`
	RegistrationDay string     `json:"registration_day,omitempty"`
}

type Event struct {
	ID     string `json:"id"`
	UserID int64  `json:"user_id"`
	Action string `json:"action"`
	Text   string `json:"text"`
}

type Platform interface {
	IsMember(context.Context, int64, int64) (bool, error)
	Send(context.Context, int64, Message) error
}

func callback(text, payload string) Button {
	return Button{Type: "callback", Text: text, Payload: payload}
}

func menu() [][]Button {
	return [][]Button{
		{callback("Получить промокод", "promo")},
		{callback("Участвовать в розыгрыше", "enter")},
		{callback("Как участвовать", "help"), callback("Мои шансы", "chances")},
		{callback("Правила акции", "rules")},
	}
}

func subscriptionButtons(d Day) [][]Button {
	return [][]Button{{{Type: "link", Text: "Подписаться на " + d.Partner, URL: d.URL}}, {callback("Проверить подписку", "check:"+d.Date)}, {callback("В меню", "start")}}
}
