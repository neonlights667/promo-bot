package bot

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"
)

type Day struct {
	Date             string   `json:"date"`
	Partner          string   `json:"partner"`
	ChannelID        int64    `json:"channel_id"`
	URL              string   `json:"url"`
	PromoUntil       string   `json:"promo_until"`
	PromoUsage       string   `json:"promo_usage"`
	Prizes           []string `json:"prizes"`
	ClaimInstruction string   `json:"claim_instruction"`
}

type Config struct {
	Timezone     string         `json:"timezone"`
	Rules        string         `json:"rules"`
	Consent      string         `json:"consent"`
	BannerURL    string         `json:"banner_url"`
	GrandPrize   string         `json:"grand_prize"`
	ReserveCount int            `json:"reserve_count"`
	ClaimHours   int            `json:"claim_hours"`
	Days         []Day          `json:"days"`
	Location     *time.Location `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	if err != nil {
		return c, err
	}
	if err = c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) Validate() error {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return err
	}
	c.Location = loc
	if len(c.Days) != 10 || c.ClaimHours < 1 || c.ClaimHours > 8760 || c.ReserveCount < 1 || c.ReserveCount > 100 || c.Rules == "" || c.Consent == "" || c.GrandPrize == "" || len([]rune(c.Rules)) > 3500 || len([]rune(c.Consent)) > 3000 {
		return fmt.Errorf("configure 10 days, rules, consent, grand prize, claim_hours and reserve_count")
	}
	for i, d := range c.Days {
		date, err := time.ParseInLocation(time.DateOnly, d.Date, loc)
		if err != nil {
			return err
		}
		if i > 0 {
			previous, _ := time.ParseInLocation(time.DateOnly, c.Days[i-1].Date, loc)
			if !date.Equal(previous.AddDate(0, 0, 1)) {
				return fmt.Errorf("campaign days must be consecutive")
			}
		}
		u, err := url.Parse(d.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || d.ChannelID == 0 || d.Partner == "" || d.PromoUsage == "" || d.PromoUntil == "" || d.ClaimInstruction == "" || len(d.Prizes) == 0 {
			return fmt.Errorf("incomplete partner settings for %s", d.Date)
		}
		for _, p := range d.Prizes {
			if p == "" || len([]rune(p)) > 300 {
				return fmt.Errorf("empty prize for %s", d.Date)
			}
		}
		if len([]rune(d.Partner)) > 40 || len([]rune(d.PromoUntil)) > 100 || len([]rune(d.PromoUsage)) > 2000 || len([]rune(d.ClaimInstruction)) > 2000 || len(d.Prizes) > 1000 {
			return fmt.Errorf("partner settings exceed message limits for %s", d.Date)
		}
	}
	if len([]rune(c.GrandPrize)) > 300 {
		return fmt.Errorf("grand_prize is too long")
	}
	if c.BannerURL != "" {
		u, err := url.Parse(c.BannerURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("banner_url must be HTTPS")
		}
	}
	return nil
}

func (c Config) Current(now time.Time) (Day, bool) {
	date := now.In(c.Location).Format(time.DateOnly)
	for _, d := range c.Days {
		if d.Date == date {
			return d, true
		}
	}
	return Day{}, false
}

func (c Config) Day(date string) (Day, bool) {
	for _, d := range c.Days {
		if d.Date == date {
			return d, true
		}
	}
	return Day{}, false
}
