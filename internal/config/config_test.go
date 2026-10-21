package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/config"
)

// A secret long enough to pass the minimum-length check.
const validSecret = "0123456789abcdef0123456789abcdef" // 32 chars

// setEnv applies environment variables for the duration of a test. t.Setenv
// restores the previous value afterwards and forbids t.Parallel, which is the
// right trade here: config is read from the process environment, so tests must
// not share it.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// unsetEnv removes a variable for the duration of a test. t.Setenv cannot
// express "unset" (an empty value is still set), yet DATA_DIR deliberately
// distinguishes the two: unset means "use the default", set-but-blank is an
// error.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	})
}

// validEnv returns a baseline environment that Load must accept.
func validEnv(t *testing.T) {
	t.Helper()
	setEnv(t, map[string]string{
		"MAIL_DOMAINS":   "mail.example.com,mail.example.org",
		"INBOUND_SECRET": validSecret,
		// Clear anything the outer environment may have set, so defaults apply.
		"LISTEN_ADDR":       "",
		"APP_NAME":          "",
		"MAX_INBOUND_BYTES": "",
		"LOG_LEVEL":         "",
	})
	unsetEnv(t, "DATA_DIR")
}

func TestLoadDefaults(t *testing.T) {
	validEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if cfg.ListenAddr != config.DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, config.DefaultListenAddr)
	}
	if cfg.DataDir != config.DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, config.DefaultDataDir)
	}
	wantDB := filepath.Join(config.DefaultDataDir, "db.sql")
	if cfg.DBPath != wantDB {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, wantDB)
	}
	if cfg.AppName != config.DefaultAppName {
		t.Errorf("AppName = %q, want %q", cfg.AppName, config.DefaultAppName)
	}
	if cfg.MaxInboundBytes != 25<<20 {
		t.Errorf("MaxInboundBytes = %d, want %d", cfg.MaxInboundBytes, 25<<20)
	}
}

// TestDataDirDerivesAbsoluteDBPath pins the two guarantees operators rely on:
// the database is always $DATA_DIR/db.sql, and a relative DATA_DIR is resolved
// against the startup working directory so a later chdir cannot move it.
func TestDataDirDerivesAbsoluteDBPath(t *testing.T) {
	validEnv(t)
	setEnv(t, map[string]string{"DATA_DIR": "./relative-data"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("DataDir = %q, want an absolute path", cfg.DataDir)
	}
	wantDB := filepath.Join(cfg.DataDir, "db.sql")
	if cfg.DBPath != wantDB {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, wantDB)
	}
}

func TestLoadDomainsAreLowercasedAndTrimmed(t *testing.T) {
	validEnv(t)
	setEnv(t, map[string]string{
		"MAIL_DOMAINS": "  MAIL.Example.COM ,, mail.example.org ",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	want := []string{"mail.example.com", "mail.example.org"}
	if strings.Join(cfg.MailDomains, ",") != strings.Join(want, ",") {
		t.Errorf("MailDomains = %v, want %v", cfg.MailDomains, want)
	}
	if cfg.DefaultDomain() != "mail.example.com" {
		t.Errorf("DefaultDomain() = %q, want the first entry", cfg.DefaultDomain())
	}
	if !cfg.HasDomain("mail.example.org") {
		t.Error("HasDomain(mail.example.org) = false")
	}
	if cfg.HasDomain("other.com") {
		t.Error("HasDomain(other.com) = true")
	}
}

// The reference implementation lowercased request domains but not configured
// ones, so an uppercase MAIL_DOMAIN silently rejected every explicit domain.
// Confirm both sides are normalised here.
func TestHasDomainMatchesLowercasedRequest(t *testing.T) {
	validEnv(t)
	setEnv(t, map[string]string{"MAIL_DOMAINS": "MAIL.EXAMPLE.COM"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !cfg.HasDomain("mail.example.com") {
		t.Error("uppercase-configured domain does not match a lowercase request")
	}
}

func TestLoadMaxInboundBytesFormats(t *testing.T) {
	cases := map[string]int64{
		"1024":   1024,
		"1KB":    1 << 10,
		"25MB":   25 << 20,
		"1GB":    1 << 30,
		"2048B":  2048,
		" 4096 ": 4096,
		"2mb":    2 << 20,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			validEnv(t)
			setEnv(t, map[string]string{"MAX_INBOUND_BYTES": raw})

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if cfg.MaxInboundBytes != want {
				t.Errorf("MaxInboundBytes = %d, want %d", cfg.MaxInboundBytes, want)
			}
		})
	}
}

// TestLoadRejectsInvalidConfig covers every startup validation failure. The
// point of these is that the process refuses to boot rather than running an
// unsafe or misconfigured server.
func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := map[string]struct {
		env     map[string]string
		wantErr string
	}{
		"missing domains": {
			env:     map[string]string{"MAIL_DOMAINS": ""},
			wantErr: "MAIL_DOMAINS",
		},
		"only separators and spaces": {
			env:     map[string]string{"MAIL_DOMAINS": " , , "},
			wantErr: "MAIL_DOMAINS",
		},
		"missing secret": {
			env:     map[string]string{"INBOUND_SECRET": ""},
			wantErr: "INBOUND_SECRET must be set",
		},
		"secret too short": {
			env:     map[string]string{"INBOUND_SECRET": "short"},
			wantErr: "at least 32 characters",
		},
		"secret exactly 31 chars": {
			env:     map[string]string{"INBOUND_SECRET": strings.Repeat("a", 31)},
			wantErr: "at least 32 characters",
		},
		"domain without dot": {
			env:     map[string]string{"MAIL_DOMAINS": "localhost"},
			wantErr: "must contain at least one dot",
		},
		"domain with underscore": {
			env:     map[string]string{"MAIL_DOMAINS": "mail_example.com"},
			wantErr: "invalid character",
		},
		"domain with empty label": {
			env:     map[string]string{"MAIL_DOMAINS": "mail..example.com"},
			wantErr: "empty label",
		},
		"duplicate domains": {
			env:     map[string]string{"MAIL_DOMAINS": "a.example.com,a.example.com"},
			wantErr: "duplicate domain",
		},
		"domain too long": {
			env:     map[string]string{"MAIL_DOMAINS": strings.Repeat("a.", 130) + "com"},
			wantErr: "exceeds 253 characters",
		},
		"label too long": {
			env:     map[string]string{"MAIL_DOMAINS": strings.Repeat("a", 64) + ".com"},
			wantErr: "label longer than 63",
		},
		"leading hyphen label": {
			env:     map[string]string{"MAIL_DOMAINS": "-mail.example.com"},
			wantErr: "starting or ending with a hyphen",
		},
		"invalid listen address": {
			env:     map[string]string{"LISTEN_ADDR": "not-an-address"},
			wantErr: "LISTEN_ADDR",
		},
		"empty data dir": {
			env:     map[string]string{"DATA_DIR": "  "},
			wantErr: "DATA_DIR",
		},
		"invalid max inbound bytes": {
			env:     map[string]string{"MAX_INBOUND_BYTES": "lots"},
			wantErr: "MAX_INBOUND_BYTES",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Start from a valid baseline, then apply only the broken value.
			validEnv(t)
			setEnv(t, tc.env)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("Load() succeeded; want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Load() error = %q; want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestMissingSecretExplainsHowToFix asserts the operator gets an actionable
// message, since this is the most likely misconfiguration.
func TestMissingSecretExplainsHowToFix(t *testing.T) {
	validEnv(t)
	setEnv(t, map[string]string{"INBOUND_SECRET": ""})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() succeeded without a secret")
	}
	msg := err.Error()
	for _, want := range []string{"refusing to start", "openssl rand -hex 32"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q", msg, want)
		}
	}
}

func TestAcceptsValidListenAddresses(t *testing.T) {
	for _, addr := range []string{":8080", "127.0.0.1:8080", "0.0.0.0:9000", "[::1]:8080"} {
		t.Run(addr, func(t *testing.T) {
			validEnv(t)
			setEnv(t, map[string]string{"LISTEN_ADDR": addr})

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if cfg.ListenAddr != addr {
				t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, addr)
			}
		})
	}
}
