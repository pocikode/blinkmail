// Package config loads and validates Blinkmail runtime configuration.
//
// Configuration comes from environment variables only. Invalid or missing
// values cause startup to fail with a clear error rather than silently running
// an unsafe server (notably: no unauthenticated public ingestion endpoint).
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Configuration defaults.
const (
	DefaultListenAddr = ":8080"
	DefaultDBPath     = "/data/blinkmail.db"
	DefaultAppName    = "Blinkmail"
	DefaultWebHost    = "localhost:8080"
)

// Config holds all validated runtime settings.
type Config struct {
	// ListenAddr is the HTTP listen address, e.g. ":8080".
	ListenAddr string
	// DBPath is the SQLite database file path.
	DBPath string
	// AppName is the user-visible application name.
	AppName string
	// MailDomains lists accepted mail domains; MailDomains[0] is the default.
	MailDomains []string
	// WebHost is the public host of the web UI, shown to users.
	WebHost string
	// InboundSecret is the shared bearer secret the Email Worker must present
	// when calling POST /api/inbound.
	InboundSecret string
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
	cfg := Config{
		ListenAddr:      getEnv("BLINKMAIL_LISTEN_ADDR", DefaultListenAddr),
		DBPath:          getEnv("BLINKMAIL_DB_PATH", DefaultDBPath),
		AppName:         getEnv("BLINKMAIL_APP_NAME", DefaultAppName),
		WebHost:         getEnv("BLINKMAIL_WEB_HOST", DefaultWebHost),
		InboundSecret:   os.Getenv("BLINKMAIL_INBOUND_SECRET"),
		MaxInboundBytes: 25 << 20, // 25 MiB
	}

	if size := os.Getenv("BLINKMAIL_MAX_INBOUND_BYTES"); size != "" {
		n, err := parseSize(size)
		if err != nil {
			return Config{}, fmt.Errorf("BLINKMAIL_MAX_INBOUND_BYTES: %w", err)
		}
		cfg.MaxInboundBytes = n
	}

	domains, err := parseDomains(os.Getenv("BLINKMAIL_MAIL_DOMAINS"))
	if err != nil {
		return Config{}, err
	}
	cfg.MailDomains = domains

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects configurations that would be unsafe or unbootable.
func (c Config) validate() error {
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("BLINKMAIL_LISTEN_ADDR %q is not a valid listen address: %w", c.ListenAddr, err)
	}
	if c.DBPath == "" {
		return fmt.Errorf("BLINKMAIL_DB_PATH must not be empty")
	}
	if c.AppName == "" {
		return fmt.Errorf("BLINKMAIL_APP_NAME must not be empty")
	}
	if c.WebHost == "" {
		return fmt.Errorf("BLINKMAIL_WEB_HOST must not be empty")
	}

	// A missing or weak ingestion secret would expose a public endpoint that
	// accepts arbitrary mail. Refuse to start instead.
	switch {
	case c.InboundSecret == "":
		return fmt.Errorf("BLINKMAIL_INBOUND_SECRET must be set: refusing to start an unauthenticated inbound mail endpoint (generate one with: openssl rand -hex 32)")
	case len(c.InboundSecret) < 32:
		return fmt.Errorf("BLINKMAIL_INBOUND_SECRET must be at least 32 characters, got %d", len(c.InboundSecret))
	}

	if c.MaxInboundBytes <= 0 {
		return fmt.Errorf("BLINKMAIL_MAX_INBOUND_BYTES must be positive, got %d", c.MaxInboundBytes)
	}

	return nil
}

// parseDomains splits a comma-separated domain list, lowercases entries,
// drops empties, and rejects duplicates and malformed domains.
func parseDomains(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("BLINKMAIL_MAIL_DOMAINS must list at least one domain, e.g. \"mail.example.com,mail.example.org\"")
	}

	var domains []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		domain := strings.ToLower(strings.TrimSpace(part))
		if domain == "" {
			continue
		}
		if err := validateDomain(domain); err != nil {
			return nil, fmt.Errorf("BLINKMAIL_MAIL_DOMAINS: %w", err)
		}
		if _, dup := seen[domain]; dup {
			return nil, fmt.Errorf("BLINKMAIL_MAIL_DOMAINS: duplicate domain %q", domain)
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}

	if len(domains) == 0 {
		return nil, fmt.Errorf("BLINKMAIL_MAIL_DOMAINS must list at least one non-empty domain")
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
