//go:build xendit_cert

package xendit

import (
	"context"
	"testing"
)

func TestXenditCertificationCredentials(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatal("XENDIT_SECRET_API_KEY is required for xendit certification tests")
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.ValidateCredentials(context.Background()); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
}
