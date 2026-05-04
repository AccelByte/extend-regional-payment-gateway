//go:build komoju_cert

package komoju

import (
	"context"
	"testing"
)

func TestKomojuCertificationCredentials(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatal("KOMOJU_SECRET_KEY is required for komoju certification tests")
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.ValidateCredentials(context.Background()); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
}
