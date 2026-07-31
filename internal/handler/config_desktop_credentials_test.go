package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/desktopcredentials"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type handlerCredentialStore struct {
	values   map[string]string
	setError error
}

func (s *handlerCredentialStore) Get(account string) (string, error) {
	value, ok := s.values[account]
	if !ok {
		return "", errors.New("credential not found")
	}
	return value, nil
}

func (s *handlerCredentialStore) Set(account, secret string) error {
	if s.setError != nil {
		return s.setError
	}
	s.values[account] = secret
	return nil
}

func (s *handlerCredentialStore) Delete(account string) error {
	delete(s.values, account)
	return nil
}

func TestDesktopConfigResponseRedactsSecretsAndEmptyUpdatePreservesThem(t *testing.T) {
	handler, cfg, configPath, _ := newDesktopConfigHandler(t)

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	handler.GetConfig(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GetConfig status = %d: %s", getRecorder.Code, getRecorder.Body.String())
	}
	var response GetConfigResponse
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AI.Channels["primary"].APIKey != "" || response.FOFA.APIKey != "" {
		t.Fatal("desktop config response exposed a secret")
	}
	if !response.CredentialStatus[desktopcredentials.AIChannelPath("primary")] || !response.CredentialStatus[desktopcredentials.PathFOFA] {
		t.Fatalf("desktop config response omitted protected credential status: %v", response.CredentialStatus)
	}

	body := []byte(`{
  "ai": {
    "default_channel": "primary",
    "channels": {
      "primary": {
        "api_key": "",
        "base_url": "https://ai.example/v1",
        "model": "updated-model"
      }
    }
  },
  "fofa": {
    "api_key": "",
    "base_url": "https://fofa.example/api"
  }
}`)
	recorder := updateDesktopConfig(t, handler, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("UpdateConfig status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.AI.Channels["primary"].APIKey != "ai-secret" || cfg.FOFA.APIKey != "fofa-secret" {
		t.Fatal("empty desktop credential update cleared an existing secret")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("ai-secret")) || bytes.Contains(data, []byte("fofa-secret")) {
		t.Fatalf("desktop config persisted plaintext: %s", data)
	}
	if !bytes.Contains(data, []byte(desktopcredentials.ReferencePrefix)) {
		t.Fatalf("desktop config did not persist credential references: %s", data)
	}
}

func TestDesktopConfigUpdateStoresNewDefaultChannelSecret(t *testing.T) {
	handler, cfg, configPath, store := newDesktopConfigHandler(t)
	body := []byte(`{
  "ai": {
    "default_channel": "primary",
    "channels": {
      "primary": {
        "api_key": "new-ai-secret",
        "base_url": "https://ai.example/v1",
        "model": "new-model"
      }
    }
  }
}`)
	recorder := updateDesktopConfig(t, handler, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("UpdateConfig status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.AI.Channels["primary"].APIKey != "new-ai-secret" || cfg.OpenAI.APIKey != "new-ai-secret" {
		t.Fatal("runtime default AI credential was not updated")
	}
	found := false
	for _, value := range store.values {
		if value == "new-ai-secret" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("new AI credential was not written to the credential store")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("new-ai-secret")) || !bytes.Contains(data, []byte(desktopcredentials.ReferencePrefix)) {
		t.Fatalf("new AI credential was not safely persisted: %s", data)
	}
}

func TestDesktopConfigUpdateFailsBeforeMutationWhenCredentialStoreFails(t *testing.T) {
	handler, cfg, _, store := newDesktopConfigHandler(t)
	store.setError = errors.New("keychain locked")
	body := []byte(`{"fofa":{"api_key":"replacement","base_url":"https://new.example"}}`)
	recorder := updateDesktopConfig(t, handler, body)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateConfig status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.FOFA.APIKey != "fofa-secret" || cfg.FOFA.BaseURL != "https://fofa.example" {
		t.Fatal("failed credential update mutated runtime config")
	}
	if strings.Contains(recorder.Body.String(), "replacement") || strings.Contains(recorder.Body.String(), "keychain locked") {
		t.Fatal("credential failure response exposed sensitive details")
	}
}

func TestDesktopConfigUpdateRestoresRuntimeSecretsWhenPersistenceFails(t *testing.T) {
	handler, cfg, _, store := newDesktopConfigHandler(t)
	handler.configPath = filepath.Join(t.TempDir(), "missing", "config.yaml")
	storedBefore := len(store.values)
	body := []byte(`{"fofa":{"api_key":"replacement","base_url":"https://new.example"}}`)
	recorder := updateDesktopConfig(t, handler, body)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateConfig status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.FOFA.APIKey != "fofa-secret" {
		t.Fatalf("runtime secret after persistence failure = %q", cfg.FOFA.APIKey)
	}
	if len(store.values) != storedBefore {
		t.Fatalf("credential store retained a rolled-back value: %v", store.values)
	}
	for _, value := range store.values {
		if value == "replacement" {
			t.Fatal("credential store retained replacement after persistence failure")
		}
	}
}

func newDesktopConfigHandler(t *testing.T) (*ConfigHandler, *config.Config, string, *handlerCredentialStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	initial := []byte(`ai:
  default_channel: primary
  channels:
    primary:
      api_key: ai-secret
      base_url: https://ai.example/v1
      model: initial-model
fofa:
  api_key: fofa-secret
  base_url: https://fofa.example
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AI: config.AIConfig{
			DefaultChannel: "primary",
			Channels: map[string]config.AIChannelConfig{
				"primary": {
					APIKey:  "ai-secret",
					BaseURL: "https://ai.example/v1",
					Model:   "initial-model",
				},
			},
		},
		OpenAI: config.OpenAIConfig{
			APIKey:  "ai-secret",
			BaseURL: "https://ai.example/v1",
			Model:   "initial-model",
		},
		FOFA: config.FofaConfig{APIKey: "fofa-secret", BaseURL: "https://fofa.example"},
	}
	store := &handlerCredentialStore{values: make(map[string]string)}
	manager, err := desktopcredentials.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ResolveAndMigrate(cfg, func([]string) error { return nil }, func(*config.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	handler := NewConfigHandler(configPath, cfg, nil, nil, nil, nil, nil, zap.NewNop())
	handler.SetDesktopCredentialManager(manager)
	return handler, cfg, configPath, store
}

func updateDesktopConfig(t *testing.T, handler *ConfigHandler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateConfig(context)
	return recorder
}
