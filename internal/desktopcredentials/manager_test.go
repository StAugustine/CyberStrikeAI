package desktopcredentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cyberstrike-ai/internal/config"
	"gopkg.in/yaml.v3"
)

type memoryStore struct {
	values   map[string]string
	setError error
	getError error
	deleted  []string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: make(map[string]string)}
}

func (s *memoryStore) Get(account string) (string, error) {
	if s.getError != nil {
		return "", s.getError
	}
	value, ok := s.values[account]
	if !ok {
		return "", errors.New("credential not found")
	}
	return value, nil
}

func (s *memoryStore) Set(account, secret string) error {
	if s.setError != nil {
		return s.setError
	}
	s.values[account] = secret
	return nil
}

func (s *memoryStore) Delete(account string) error {
	delete(s.values, account)
	s.deleted = append(s.deleted, account)
	return nil
}

func TestResolveAndMigrateProtectsPlaintextAndRedactsResponses(t *testing.T) {
	store := newMemoryStore()
	manager := deterministicManager(t, store)
	cfg := &config.Config{
		AI: config.AIConfig{Channels: map[string]config.AIChannelConfig{
			"primary": {APIKey: "ai-secret"},
		}},
		FOFA: config.FofaConfig{APIKey: "fofa-secret"},
	}
	var persisted *config.Config
	if err := manager.ResolveAndMigrate(cfg, func(paths []string) error {
		want := []string{"ai.channels.primary.api_key", "fofa.api_key"}
		if fmt.Sprint(paths) != fmt.Sprint(want) {
			t.Fatalf("migration paths = %v, want %v", paths, want)
		}
		return nil
	}, func(replacement *config.Config) error {
		persisted = replacement
		return nil
	}); err != nil {
		t.Fatalf("ResolveAndMigrate: %v", err)
	}
	if cfg.AI.Channels["primary"].APIKey != "ai-secret" || cfg.FOFA.APIKey != "fofa-secret" {
		t.Fatal("runtime config did not retain resolved secrets")
	}
	if persisted == nil {
		t.Fatal("plaintext migration did not persist references")
	}
	if got := persisted.AI.Channels["primary"].APIKey; !strings.HasPrefix(got, ReferencePrefix) {
		t.Fatalf("AI key was not replaced with a reference: %q", got)
	}
	if got := persisted.FOFA.APIKey; !strings.HasPrefix(got, ReferencePrefix) {
		t.Fatalf("FOFA key was not replaced with a reference: %q", got)
	}
	persistedData, err := yaml.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persistedData), "ai-secret") || strings.Contains(string(persistedData), "fofa-secret") {
		t.Fatal("plaintext secret remained in persisted config")
	}

	redacted, err := manager.RedactedCopy(cfg)
	if err != nil {
		t.Fatalf("RedactedCopy: %v", err)
	}
	if redacted.AI.Channels["primary"].APIKey != "" || redacted.FOFA.APIKey != "" {
		t.Fatal("redacted config exposed a credential")
	}
	protected, err := manager.PersistenceCopy(cfg)
	if err != nil {
		t.Fatalf("PersistenceCopy: %v", err)
	}
	if protected.AI.Channels["primary"].APIKey != persisted.AI.Channels["primary"].APIKey || protected.FOFA.APIKey != persisted.FOFA.APIKey {
		t.Fatal("persistence copy did not reuse committed references")
	}
}

func TestResolveAndMigrateReadsExistingReferenceWithoutRewritingConfig(t *testing.T) {
	store := newMemoryStore()
	store.values["existing-account"] = "resolved-secret"
	manager := deterministicManager(t, store)
	cfg := &config.Config{OpenAI: config.OpenAIConfig{APIKey: ReferencePrefix + "existing-account"}}
	persisted := false
	if err := manager.ResolveAndMigrate(cfg, nil, func(*config.Config) error {
		persisted = true
		return nil
	}); err != nil {
		t.Fatalf("ResolveAndMigrate: %v", err)
	}
	if persisted {
		t.Fatal("existing reference unexpectedly rewrote config")
	}
	if cfg.OpenAI.APIKey != "resolved-secret" {
		t.Fatalf("reference resolved to %q", cfg.OpenAI.APIKey)
	}
}

func TestResolveAndMigrateSharesDefaultAICompatibilityCredential(t *testing.T) {
	store := newMemoryStore()
	manager := deterministicManager(t, store)
	cfg := &config.Config{
		AI: config.AIConfig{
			DefaultChannel: "primary",
			Channels: map[string]config.AIChannelConfig{
				"primary": {APIKey: "shared-secret"},
			},
		},
		OpenAI: config.OpenAIConfig{APIKey: "shared-secret"},
	}
	var persisted *config.Config
	if err := manager.ResolveAndMigrate(cfg, func(paths []string) error {
		if len(paths) != 1 || paths[0] != AIChannelPath("primary") {
			t.Fatalf("unexpected migration paths: %v", paths)
		}
		return nil
	}, func(replacement *config.Config) error {
		persisted = replacement
		return nil
	}); err != nil {
		t.Fatalf("ResolveAndMigrate: %v", err)
	}
	if len(store.values) != 1 {
		t.Fatalf("default AI compatibility alias created duplicate credentials: %v", store.values)
	}
	if persisted.OpenAI.APIKey != persisted.AI.Channels["primary"].APIKey {
		t.Fatalf("compatibility fields used different references: %#v", persisted)
	}
	redacted, err := manager.RedactedCopy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if redacted.OpenAI.APIKey != "" || redacted.AI.Channels["primary"].APIKey != "" {
		t.Fatal("redacted config exposed the shared credential")
	}
	protected, err := manager.PersistenceCopy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if protected.OpenAI.APIKey != protected.AI.Channels["primary"].APIKey {
		t.Fatal("persistence copy did not preserve the shared reference")
	}
	reloadedManager := deterministicManager(t, store)
	if err := reloadedManager.ResolveAndMigrate(persisted, nil, func(*config.Config) error {
		t.Fatal("existing shared reference unexpectedly rewrote config")
		return nil
	}); err != nil {
		t.Fatalf("reload shared reference: %v", err)
	}
	if persisted.OpenAI.APIKey != "shared-secret" || persisted.AI.Channels["primary"].APIKey != "shared-secret" {
		t.Fatal("shared credential reference did not resolve into both runtime views")
	}
}

func TestResolveAndMigrateRollsBackCredentialWhenPersistenceFails(t *testing.T) {
	store := newMemoryStore()
	manager := deterministicManager(t, store)
	cfg := &config.Config{Vision: config.VisionConfig{APIKey: "vision-secret"}}
	err := manager.ResolveAndMigrate(cfg, func([]string) error { return nil }, func(*config.Config) error {
		return errors.New("disk unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "persist desktop credential references") {
		t.Fatalf("unexpected migration error: %v", err)
	}
	if len(store.values) != 0 || len(store.deleted) != 1 {
		t.Fatalf("new credential was not rolled back: values=%v deleted=%v", store.values, store.deleted)
	}
}

func TestResolveAndMigrateRequiresApprovalBeforeCredentialWrites(t *testing.T) {
	store := newMemoryStore()
	manager := deterministicManager(t, store)
	cfg := &config.Config{FOFA: config.FofaConfig{APIKey: "migration-secret"}}
	persisted := false
	err := manager.ResolveAndMigrate(cfg, func(paths []string) error {
		if len(paths) != 1 || paths[0] != PathFOFA {
			t.Fatalf("unexpected migration paths: %v", paths)
		}
		for _, path := range paths {
			if strings.Contains(path, "migration-secret") {
				t.Fatal("migration approval exposed plaintext")
			}
		}
		return errors.New("migration declined")
	}, func(*config.Config) error {
		persisted = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "migration declined") {
		t.Fatalf("unexpected approval error: %v", err)
	}
	if len(store.values) != 0 || len(store.deleted) != 0 {
		t.Fatalf("credential store changed before approval: values=%v deleted=%v", store.values, store.deleted)
	}
	if persisted {
		t.Fatal("config persisted before migration approval")
	}
}

func TestPersistenceCopyFailsClosedForUnprotectedSecret(t *testing.T) {
	manager := deterministicManager(t, newMemoryStore())
	_, err := manager.PersistenceCopy(&config.Config{Shodan: config.SpaceSearchConfig{APIKey: "unprotected"}})
	if err == nil || !strings.Contains(err.Error(), PathShodan) {
		t.Fatalf("unexpected persistence result: %v", err)
	}
}

func TestUpdatePreservesEmptyInputAndRollsBackNewCredential(t *testing.T) {
	store := newMemoryStore()
	manager := deterministicManager(t, store)
	cfg := &config.Config{FOFA: config.FofaConfig{APIKey: "old-secret"}}
	if err := manager.ResolveAndMigrate(cfg, func([]string) error { return nil }, func(*config.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}

	update := manager.BeginUpdate()
	empty := ""
	if err := update.Protect(PathFOFA, &empty, cfg.FOFA.APIKey); err != nil {
		t.Fatal(err)
	}
	if empty != "old-secret" {
		t.Fatalf("empty input did not preserve current secret: %q", empty)
	}
	newSecret := "new-secret"
	if err := update.Protect(PathFOFA, &newSecret, cfg.FOFA.APIKey); err != nil {
		t.Fatal(err)
	}
	if err := update.Activate(); err != nil {
		t.Fatal(err)
	}
	cfg.FOFA.APIKey = newSecret
	protected, err := manager.PersistenceCopy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	newReference := protected.FOFA.APIKey
	update.Rollback()
	protected, err = manager.PersistenceCopy(&config.Config{FOFA: config.FofaConfig{APIKey: "old-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if protected.FOFA.APIKey == newReference {
		t.Fatal("rollback retained the staged credential reference")
	}
	account, _, _ := ParseReference(newReference)
	if _, exists := store.values[account]; exists {
		t.Fatal("rollback retained the staged credential value")
	}
}

func TestWriteConfigAtomicallyUsesReferenceAndPrivateMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	cfg := &config.Config{FOFA: config.FofaConfig{APIKey: ReferencePrefix + "account-one"}}
	if err := WriteConfigAtomically(path, cfg); err != nil {
		t.Fatalf("WriteConfigAtomically: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), ReferencePrefix+"account-one") {
		t.Fatalf("credential reference missing from config: %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func deterministicManager(t *testing.T, store Store) *Manager {
	t.Helper()
	next := 0
	manager, err := newManager(store, func() (string, error) {
		next++
		return fmt.Sprintf("account-%d", next), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
