// Command blinkmail runs the Blinkmail disposable-mail HTTP server.
//
// It wires configuration, SQLite persistence, the JSON API, and the embedded
// frontend together, then serves until it receives SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pocikode/blinkmail/internal/api"
	"pocikode/blinkmail/internal/auth"
	"pocikode/blinkmail/internal/config"
	"pocikode/blinkmail/internal/store"
	"pocikode/blinkmail/internal/web"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests before the listener is closed outright.
const shutdownTimeout = 15 * time.Second

// healthcheckTimeout bounds the container self-probe.
const healthcheckTimeout = 4 * time.Second

func main() {
	// The container image has no shell or curl, so the Dockerfile HEALTHCHECK
	// re-invokes the binary with -healthcheck to probe the local /healthz.
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()

	if *healthcheck {
		if err := probeHealth(); err != nil {
			fmt.Fprintf(os.Stderr, "blinkmail: healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// Structured logging is not up yet when configuration fails, so write
		// straight to stderr with a clear, actionable message.
		fmt.Fprintf(os.Stderr, "blinkmail: %v\n", err)
		os.Exit(1)
	}
}

// probeHealth performs a local HTTP GET against /healthz and reports whether
// the server answered 200. It reads the listen address from the same
// configuration the server uses so the probe targets the right port.
func probeHealth() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", cfg.ListenAddr, err)
	}
	// An empty host binds all interfaces; probe via loopback instead.
	if host == "" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned %s", resp.Status)
	}
	return nil
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

	if err := bootstrapPassword(context.Background(), db, cfg, logger); err != nil {
		return err
	}

	// Expired login sessions are dead weight; sweep them once at startup rather
	// than on every request. A failure here is not worth refusing to boot for.
	if err := db.DeleteExpiredAuthSessions(context.Background()); err != nil {
		logger.Warn("could not delete expired auth sessions", "error", err)
	}

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
			"data_dir", cfg.DataDir,
			"database", cfg.DBPath,
			"domains", cfg.MailDomains,
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

// bootstrapPassword seeds the admin password on the very first start.
//
// INITIAL_PASSWORD is a one-time seed, not configuration: once a hash exists in
// the database the environment is ignored, so rotating the env var on a running
// deployment cannot silently reset the password. Changes are made from the UI
// instead. The password itself is never logged.
func bootstrapPassword(ctx context.Context, db *store.Store, cfg config.Config, logger *slog.Logger) error {
	_, err := db.PasswordHash(ctx)
	switch {
	case err == nil:
		if cfg.InitialPassword != "" {
			logger.Debug("INITIAL_PASSWORD ignored: admin password already set")
		}
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("read admin password: %w", err)
	}

	if cfg.InitialPassword == "" {
		return errors.New("INITIAL_PASSWORD must be set on first start: it seeds the admin password")
	}

	hash, err := auth.Hash(cfg.InitialPassword)
	if err != nil {
		return fmt.Errorf("hash INITIAL_PASSWORD: %w", err)
	}
	if err := db.SetPasswordHash(ctx, hash); err != nil {
		return fmt.Errorf("store admin password: %w", err)
	}
	logger.Info("admin password initialised from INITIAL_PASSWORD")
	return nil
}

// logLevel reads LOG_LEVEL, defaulting to info.
func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
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
