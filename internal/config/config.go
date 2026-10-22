// Package config loads and validates Blinkmail runtime configuration.
//
// Configuration comes from environment variables only. Invalid or missing
// values cause startup to fail with a clear error rather than silently running
// an unsafe server.
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"pocikode/blinkmail/internal/auth"
)

// Configuration defaults.
const (
	DefaultListenAddr = ":8080"
	DefaultDataDir    = "/data"
	DefaultAppName    = "BlinkMail"
)

// Config holds all validated runtime settings.
type Config struct {
	// ListenAddr is the HTTP listen address, e.g. ":8080".
	ListenAddr string
	// DataDir is the directory holding persistent data; it must be writable.
	DataDir string
	// DBPath is the SQLite database file, always DataDir/db.sql.
	DBPath string
	// AppName is the user-visible application name.
	AppName string
	// MailDomains lists accepted mail domains; MailDomains[0] is the default.
	MailDomains []string
	// InitialPassword seeds the admin password on first start only. Once a
	// password exists in the database this value is ignored; main() decides
	// whether it is required, because only it can see the database.
	InitialPassword string
	// MaxInboundBytes caps the accepted raw MIME message size.
	MaxInboundBytes int64
}

// DefaultDomain returns the primary mail domain. MailDomains always holds at
// least one entry after a successful Load.
func (c Config) DefaultDomain() string {
	return c.MailDomains[0]
}

// HasDomain reports whether domain is a configured mail domain.
func (c Config) HasDomain(domain string) bool {
	for _, d := range c.MailDomains {
		if d == domain {
			return true
		}
	}
	return false
}

// Load reads configuration from the process environment and validates it.
func Load() (Config, error) {
	err := godotenv.Load()
	if err != nil {
		log.Println("WARN: failed to load .env file, using system env")
	}

	cfg := Config{
		ListenAddr:      getEnv("LISTEN_ADDR", DefaultListenAddr),
		DataDir:         dataDirEnv(),
		AppName:         getEnv("APP_NAME", DefaultAppName),
		InitialPassword: os.Getenv("INITIAL_PASSWORD"),
		MaxInboundBytes: 25 << 20, // 25 MiB
	}

	if size := os.Getenv("MAX_INBOUND_BYTES"); size != "" {
		n, err := parseSize(size)
		if err != nil {
			return Config{}, fmt.Errorf("MAX_INBOUND_BYTES: %w", err)
		}
		cfg.MaxInboundBytes = n
	}

	domains, err := parseDomains(os.Getenv("MAIL_DOMAINS"))
	if err != nil {
		return Config{}, err
	}
	cfg.MailDomains = domains

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	// Resolve to an absolute path now: the process may chdir later.
	absDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("DATA_DIR %q: %w", cfg.DataDir, err)
	}
	cfg.DataDir = absDir
	// The database location is derived from DATA_DIR rather than separately
	// configurable, so persistent storage has a single source of truth.
	cfg.DBPath = filepath.Join(absDir, "db.sql")

	return cfg, nil
}

// validate rejects configurations that would be unsafe or unbootable.
func (c Config) validate() error {
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("LISTEN_ADDR %q is not a valid listen address: %w", c.ListenAddr, err)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("DATA_DIR must not be empty")
	}
	if c.AppName == "" {
		return fmt.Errorf("APP_NAME must not be empty")
	}

	// Inbound ingestion is authenticated with a managed API key created in the
	// web UI, so there is no secret to validate here: a fresh instance simply
	// has no keys and the relay cannot deliver until an operator makes one.
	if c.MaxInboundBytes <= 0 {
		return fmt.Errorf("MAX_INBOUND_BYTES must be positive, got %d", c.MaxInboundBytes)
	}

	// INITIAL_PASSWORD is only a seed for the first start, so it is optional
	// here: whether it is required depends on the database, which main() checks.
	// When it is set it must satisfy the same policy as a password set in the UI.
	// The value is not trimmed: leading or trailing spaces are part of the
	// password a user typed.
	if c.InitialPassword != "" {
		if err := auth.ValidatePassword(c.InitialPassword); err != nil {
			return fmt.Errorf("INITIAL_PASSWORD: %w", err)
		}
	}

	return nil
}

// parseDomains splits a comma-separated domain list, lowercases entries,
// drops empties, and rejects duplicates and malformed domains.
func parseDomains(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("MAIL_DOMAINS must list at least one domain, e.g. \"mail.example.com,mail.example.org\"")
	}

	var domains []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		domain := strings.ToLower(strings.TrimSpace(part))
		if domain == "" {
			continue
		}
		if err := validateDomain(domain); err != nil {
			return nil, fmt.Errorf("MAIL_DOMAINS: %w", err)
		}
		if _, dup := seen[domain]; dup {
			return nil, fmt.Errorf("MAIL_DOMAINS: duplicate domain %q", domain)
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}

	if len(domains) == 0 {
		return nil, fmt.Errorf("MAIL_DOMAINS must list at least one non-empty domain")
	}
	return domains, nil
}

// validateDomain checks a bare domain name: labels of alphanumerics/hyphens,
// not starting or ending with a hyphen, at least two labels, <=253 chars.
func validateDomain(domain string) error {
	if len(domain) > 253 {
		return fmt.Errorf("domain %q exceeds 253 characters", domain)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("domain %q must contain at least one dot, e.g. mail.example.com", domain)
	}
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("domain %q has an empty label", domain)
		}
		if len(label) > 63 {
			return fmt.Errorf("domain %q has a label longer than 63 characters", domain)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("domain %q has a label starting or ending with a hyphen", domain)
		}
		for i := range len(label) {
			ch := label[i]
			isAlnum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
			if !isAlnum && ch != '-' {
				return fmt.Errorf("domain %q contains invalid character %q", domain, string(ch))
			}
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// dataDirEnv returns DATA_DIR, defaulting only when it is unset. A value that is
// set but blank is passed through so validate rejects it, rather than silently
// falling back to a default storage location.
func dataDirEnv() string {
	if raw, ok := os.LookupEnv("DATA_DIR"); ok {
		return strings.TrimSpace(raw)
	}
	return DefaultDataDir
}

// parseSize accepts a plain byte count or a value suffixed with KB/MB/GB.
func parseSize(raw string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return 0, fmt.Errorf("must not be empty")
	}

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "MB"):
		multiplier, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		multiplier, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "KB"):
		multiplier, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	s = strings.TrimSpace(s)
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("%q is not a valid byte count", raw)
	}
	return n * multiplier, nil
}
