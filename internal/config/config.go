// Package config loads runtime settings from environment variables.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

type Config struct {
	Env        string // "development" or "production"
	DBPath     string
	PublicAddr string // customer-facing app
	AdminAddr  string // staff admin panel — private network only
	BaseURL    string // public URL used in links (invites, etc.)

	// MasterKey wraps each business's data key. Losing it makes encrypted
	// fields unreadable, so store it in `fly secrets` and in a password manager.
	MasterKey []byte

	TwilioSID   string
	TwilioToken string
	TwilioFrom  string // shared fallback number (demo); each business gets its own once A2P is approved

	PostmarkToken string
	EmailFrom     string
}

func (c Config) Prod() bool { return c.Env == "production" }

func Load() (Config, error) {
	c := Config{
		Env:           env("APP_ENV", "development"),
		DBPath:        env("DB_PATH", "data/app.db"),
		PublicAddr:    env("PUBLIC_ADDR", ":8080"),
		BaseURL:       strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		TwilioSID:     os.Getenv("TWILIO_ACCOUNT_SID"),
		TwilioToken:   os.Getenv("TWILIO_AUTH_TOKEN"),
		TwilioFrom:    os.Getenv("TWILIO_FROM_NUMBER"),
		PostmarkToken: os.Getenv("POSTMARK_SERVER_TOKEN"),
		EmailFrom:     env("EMAIL_FROM", "Calendar Dynamics <no-reply@localhost>"),
	}

	// The admin panel never listens on a public interface. On Fly.io,
	// "fly-local-6pn" resolves to the machine's private network address, so
	// staff reach it with `fly proxy 9090:9090` (or WireGuard).
	c.AdminAddr = os.Getenv("ADMIN_ADDR")
	if c.AdminAddr == "" {
		if os.Getenv("FLY_APP_NAME") != "" {
			c.AdminAddr = "fly-local-6pn:9090"
		} else {
			c.AdminAddr = "127.0.0.1:9090"
		}
	}

	raw := os.Getenv("MASTER_KEY")
	switch {
	case raw != "":
		k, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(k) != 32 {
			return c, errors.New("MASTER_KEY must be 32 random bytes, base64-encoded (generate with: openssl rand -base64 32)")
		}
		if weakKey(k) {
			return c, errors.New("MASTER_KEY looks like a placeholder (e.g. all zeros), not random bytes; generate a new one")
		}
		c.MasterKey = k
	case c.Prod():
		return c, errors.New("MASTER_KEY is required when APP_ENV=production")
	default:
		// Fixed development-only key so local data survives restarts.
		k := sha256.Sum256([]byte("calendar-dynamics-development-key-not-for-production"))
		c.MasterKey = k[:]
	}
	return c, nil
}

// weakKey rejects obviously non-random keys, such as an all-zero buffer left
// behind by a failed generation command, or keys with very few distinct bytes.
func weakKey(k []byte) bool {
	seen := map[byte]bool{}
	for _, b := range k {
		seen[b] = true
	}
	return len(seen) < 8
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
