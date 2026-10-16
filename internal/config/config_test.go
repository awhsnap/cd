package config

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestRejectsAllZeroMasterKey(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("expected all-zero key to be rejected, got %v", err)
	}
}

func TestAcceptsRandomMasterKey(t *testing.T) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	t.Setenv("APP_ENV", "production")
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(b))
	if _, err := Load(); err != nil {
		t.Fatalf("random key rejected: %v", err)
	}
}
