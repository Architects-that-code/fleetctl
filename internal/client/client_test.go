// internal/client/client_test.go
package client

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"fleetctl/internal/config"
)

func TestNewUnknownMethod(t *testing.T) {
	_, err := New(config.Auth{Method: "bogus"})
	if err == nil {
		t.Fatalf("expected error for unknown auth method")
	}
	if !strings.Contains(err.Error(), "unknown auth.method") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewUserConfigFileMissing(t *testing.T) {
	td := t.TempDir()
	missing := filepath.Join(td, "no-such-config")
	_, err := New(config.Auth{
		Method:     "user",
		ConfigFile: missing,
		Profile:    "DEFAULT",
	})
	if err == nil {
		t.Fatalf("expected error for missing OCI config file")
	}
	if !strings.Contains(err.Error(), "OCI config file not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateInfoNilProvider(t *testing.T) {
	var c *Client = &Client{} // nil Provider
	_, err := c.ValidateInfo(context.Background())
	if err == nil {
		t.Fatalf("expected error when provider is nil")
	}
	if !strings.Contains(err.Error(), "client not initialized") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateOnNilReceiver(t *testing.T) {
	var c *Client = nil
	if err := c.Validate(context.Background()); err == nil {
		t.Fatalf("expected error when client is nil")
	}
}
