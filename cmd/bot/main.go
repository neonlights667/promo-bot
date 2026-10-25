package main

import (
	"context"
	"errors"
	"festivalbot/internal/bot"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	if err := run(); err != nil {
		slog.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func run() error {
	token := os.Getenv("BOT_TOKEN")
	secret := os.Getenv("WEBHOOK_SECRET")
	admin := os.Getenv("ADMIN_TOKEN")
	if token == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`).MatchString(secret) || len(admin) < 32 {
		return fmt.Errorf("set BOT_TOKEN, WEBHOOK_SECRET (32-256 letters, digits, _ or -) and ADMIN_TOKEN (at least 32 characters)")
	}
	c, err := bot.LoadConfig(env("CONFIG_PATH", "campaign.json"))
	if err != nil {
		return fmt.Errorf("campaign configuration: %w", err)
	}
	dbPath := env("DB_PATH", "data/bot.db")
	if err = os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return err
	}
	store, err := bot.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.DB.Close()
	telegram := bot.NewTelegram(token)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if webhook := os.Getenv("WEBHOOK_URL"); webhook != "" {
		u, parseErr := url.Parse(webhook)
		if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/webhook" {
			return fmt.Errorf("WEBHOOK_URL must be https://your-domain/webhook")
		}
		if err = telegram.SetWebhook(ctx, webhook, secret); err != nil {
			return err
		}
	}
	service := &bot.Service{Store: store, Config: c, Platform: telegram}
	api := &bot.HTTP{Service: service, Telegram: telegram, WebhookSecret: secret, AdminToken: admin}
	server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); service.Run(ctx) }()
	serverErr := make(chan error, 1)
	go func() { slog.Info("bot listening", "address", server.Addr); serverErr <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverErr:
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	<-workerDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}
