package app

import "testing"

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
