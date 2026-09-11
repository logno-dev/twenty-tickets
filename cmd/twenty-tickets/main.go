package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"twenty-tickets/internal/admin"
	"twenty-tickets/internal/config"
	"twenty-tickets/internal/delivery"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 2 * time.Second}
		res, err := client.Get("http://127.0.0.1:" + env("PORT", "8080") + "/healthz")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("unhealthy: HTTP %d", res.StatusCode)
		}
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] != "check-resend" {
		return fmt.Errorf("unknown command: %s; manage connections at /admin/", os.Args[1])
	}
	timeout, err := time.ParseDuration(env("RESEND_HTTP_TIMEOUT", resend.DefaultTimeout.String()))
	if err != nil {
		return fmt.Errorf("RESEND_HTTP_TIMEOUT must be a duration such as 30s")
	}
	receiver, err := resend.NewWithTimeout(os.Getenv("RESEND_API_KEY"), timeout)
	if err != nil {
		return fmt.Errorf("Resend configuration: %w", err)
	}
	if len(os.Args) > 1 {
		if len(os.Args) != 3 {
			return fmt.Errorf("usage: twenty-tickets check-resend EMAIL_ID")
		}
		started := time.Now()
		email, err := receiver.Receive(context.Background(), os.Args[2])
		if err != nil {
			return fmt.Errorf("Resend retrieval diagnostic: %w", err)
		}
		bytes := 0
		if email.Text != nil {
			bytes = len(*email.Text)
		}
		log.Info("Resend retrieval successful", "email_id", email.ID, "elapsed", time.Since(started).Round(time.Millisecond).String(), "plain_text_bytes", bytes)
		return nil
	}
	if os.Getenv("RESEND_WEBHOOK_SECRET") == "" {
		return fmt.Errorf("RESEND_WEBHOOK_SECRET is required")
	}
	store, err := inbox.New(env("DATA_DIR", "./data"))
	if err != nil {
		return fmt.Errorf("inbox initialization: %w", err)
	}
	settings, err := config.Open(env("DATA_DIR", "./data"))
	if err != nil {
		return fmt.Errorf("configuration storage: %w", err)
	}
	defer settings.Close()
	adminHandler, err := admin.New(settings, store, os.Getenv("ADMIN_USERNAME"), os.Getenv("ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	handler, err := webhook.New(os.Getenv("RESEND_WEBHOOK_SECRET"), receiver, store, log, settings, settings)
	if err != nil {
		return fmt.Errorf("webhook configuration: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/", adminHandler)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusSeeOther) })
	mux.Handle("/", handler)
	server := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); delivery.New(store, settings, log, settings).Run(workerCtx) }()
	defer func() { cancelWorker(); <-workerDone }()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Info("webhook service started", "address", server.Addr, "mode", "admin_routing", "resend_http_timeout", timeout.String())
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
