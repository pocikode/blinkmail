// Command blinkmail runs the Blinkmail disposable-mail HTTP server.
//
// It wires configuration, SQLite persistence, the JSON API, and the embedded
// frontend together, then serves until it receives SIGINT or SIGTERM.
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

	"pocikode/blinkmail/internal/api"
	"pocikode/blinkmail/internal/config"
	"pocikode/blinkmail/internal/store"
	"pocikode/blinkmail/internal/web"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests before the listener is closed outright.
const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// Structured logging is not up yet when configuration fails, so write
		// straight to stderr with a clear, actionable message.
		fmt.Fprintf(os.Stderr, "blinkmail: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	db, err := store.Open(cfg.DBPath, store.EmbeddedMigrations())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	assets, err := web.Handler()
	if err != nil {
		return fmt.Errorf("load frontend assets: %w", err)
	}

	handler := api.New(api.Deps{
		Config: cfg,
		Store:  db,
		Logger: logger,
		Assets: assets,
	})

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		// Bound every phase so a slow or idle client cannot hold resources.
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      150 * time.Second, // inbound mail can be large
		IdleTimeout:       120 * time.Second,
		Handler:           handler,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Stop on SIGINT/SIGTERM so container shutdown is graceful.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting Blinkmail",
			"listen", cfg.ListenAddr,
			"database", cfg.DBPath,
			"domains", cfg.MailDomains,
			"web_host", cfg.WebHost,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Forced close after the drain window; still shut the database cleanly.
		logger.Warn("graceful shutdown timed out, forcing close", "error", err)
		_ = srv.Close()
	}
	logger.Info("shutdown complete")
	return nil
}

// logLevel reads BLINKMAIL_LOG_LEVEL, defaulting to info.
func logLevel() slog.Level {
	switch os.Getenv("BLINKMAIL_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
