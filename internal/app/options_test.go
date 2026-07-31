package app

import (
	"testing"

	"cyberstrike-ai/internal/desktopcredentials"
)

func TestWithInitialAdminPasswordProviderRequiresProvider(t *testing.T) {
	if _, err := resolveOptions([]Option{WithInitialAdminPasswordProvider(nil)}); err == nil {
		t.Fatal("expected nil initial admin password provider to be rejected")
	}
}

func TestWithInitialAdminPasswordProviderIsApplied(t *testing.T) {
	provider := func() (string, error) { return "desktop-secret", nil }
	options, err := resolveOptions([]Option{WithInitialAdminPasswordProvider(provider)})
	if err != nil {
		t.Fatalf("resolveOptions: %v", err)
	}
	if options.initialAdminPasswordProvider == nil {
		t.Fatal("initial admin password provider was not applied")
	}
}

func TestWithDesktopCredentialManagerRequiresManager(t *testing.T) {
	if _, err := resolveOptions([]Option{WithDesktopCredentialManager(nil)}); err == nil {
		t.Fatal("expected nil desktop credential manager to be rejected")
	}
}

func TestWithDesktopCredentialManagerIsApplied(t *testing.T) {
	manager, err := desktopcredentials.NewManager(optionCredentialStore{})
	if err != nil {
		t.Fatal(err)
	}
	options, err := resolveOptions([]Option{WithDesktopCredentialManager(manager)})
	if err != nil {
		t.Fatalf("resolveOptions: %v", err)
	}
	if options.desktopCredentialManager != manager {
		t.Fatal("desktop credential manager was not applied")
	}
}

type optionCredentialStore struct{}

func (optionCredentialStore) Get(string) (string, error) { return "", nil }
func (optionCredentialStore) Set(string, string) error   { return nil }
func (optionCredentialStore) Delete(string) error        { return nil }
