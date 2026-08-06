package security

import (
	"path/filepath"
	"testing"

	"cyberstrike-ai/internal/database"

	"go.uber.org/zap"
)

func TestAttachRBACStoreBootstrapsAdminPassword(t *testing.T) {
	db, err := database.NewDB(filepath.Join(t.TempDir(), "auth-bootstrap.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager := NewAuthManager(12)
	generated, err := manager.AttachRBACStore(db)
	if err != nil {
		t.Fatalf("AttachRBACStore: %v", err)
	}
	if generated == "" {
		t.Fatal("expected generated admin password on first bootstrap")
	}
	if !manager.CheckUserPassword("admin", generated) {
		t.Fatal("generated password should authenticate admin")
	}

	second, err := manager.AttachRBACStore(db)
	if err != nil {
		t.Fatalf("AttachRBACStore second call: %v", err)
	}
	if second != "" {
		t.Fatalf("expected no password on second bootstrap, got %q", second)
	}
}

func TestAttachRBACStoreUsesProvidedInitialPasswordOnce(t *testing.T) {
	db, err := database.NewDB(filepath.Join(t.TempDir(), "auth-provided-bootstrap.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager := NewAuthManager(12)
	providerCalls := 0
	generated, err := manager.AttachRBACStoreWithPasswordProvider(db, func() (string, error) {
		providerCalls++
		return "desktop-secret", nil
	})
	if err != nil {
		t.Fatalf("AttachRBACStoreWithPasswordProvider: %v", err)
	}
	if generated != "" {
		t.Fatalf("provided password leaked through generated return: %q", generated)
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	if !manager.CheckUserPassword("admin", "desktop-secret") {
		t.Fatal("provided password should authenticate admin")
	}

	if _, err := manager.AttachRBACStoreWithPasswordProvider(db, func() (string, error) {
		providerCalls++
		return "replacement-secret", nil
	}); err != nil {
		t.Fatalf("second AttachRBACStoreWithPasswordProvider: %v", err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider was called for an initialized database: %d", providerCalls)
	}
}

func TestAttachRBACStoreRejectsShortProvidedPassword(t *testing.T) {
	db, err := database.NewDB(filepath.Join(t.TempDir(), "auth-short-bootstrap.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager := NewAuthManager(12)
	if _, err := manager.AttachRBACStoreWithPasswordProvider(db, func() (string, error) {
		return "short", nil
	}); err == nil {
		t.Fatal("expected a short initial password to be rejected")
	}
	needsPassword, err := db.RBACNeedsAdminPassword()
	if err != nil {
		t.Fatalf("RBACNeedsAdminPassword: %v", err)
	}
	if !needsPassword {
		t.Fatal("failed bootstrap must leave the database requiring a password")
	}
}
