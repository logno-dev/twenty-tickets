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

	"twenty-tickets/internal/delivery"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/twenty"
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
	var twentyClient *twenty.Client
	if os.Getenv("TWENTY_BASE_URL") != "" || os.Getenv("TWENTY_API_KEY") != "" {
		var err error
		twentyClient, err = twenty.New(os.Getenv("TWENTY_BASE_URL"), os.Getenv("TWENTY_API_KEY"))
		if err != nil {
			return fmt.Errorf("Twenty configuration: %w", err)
		}
	}
	if len(os.Args) > 1 {
		if os.Args[1] != "check-twenty" {
			return fmt.Errorf("unknown command: %s", os.Args[1])
		}
		if twentyClient == nil {
			return fmt.Errorf("set TWENTY_BASE_URL and TWENTY_API_KEY")
		}
		if _, err := twentyClient.Metadata(context.Background()); err != nil {
			return err
		}
		log.Info("Twenty metadata API connection successful")
		return nil
	}
	if os.Getenv("RESEND_WEBHOOK_SECRET") == "" {
		return fmt.Errorf("RESEND_WEBHOOK_SECRET is required")
	}
	recipient, err := message.NewRecipientFilter(os.Getenv("INBOUND_EMAIL_TO"))
	if err != nil {
		return err
	}
	receiver, err := resend.New(os.Getenv("RESEND_API_KEY"))
	if err != nil {
		return fmt.Errorf("Resend configuration: %w", err)
	}
	store, err := inbox.New(env("DATA_DIR", "./data"))
	if err != nil {
		return fmt.Errorf("inbox initialization: %w", err)
	}
	handler, err := webhook.New(os.Getenv("RESEND_WEBHOOK_SECRET"), receiver, store, log, recipient)
	if err != nil {
		return fmt.Errorf("webhook configuration: %w", err)
	}
	server := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	mode := "inbox_only"
	if twentyClient != nil {
		mode = "twenty_tickets"
		go func() {
			defer close(workerDone)
			delivery.New(store, twentyClient, log, recipient).Run(workerCtx)
		}()
	} else {
		close(workerDone)
	}
	defer func() { cancelWorker(); <-workerDone }()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Info("webhook service started", "address", server.Addr, "mode", mode, "twenty_configured", twentyClient != nil)
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
