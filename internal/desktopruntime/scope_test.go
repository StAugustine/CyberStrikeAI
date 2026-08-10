package desktopruntime

import (
	"testing"

	"cyberstrike-ai/internal/config"
)

func TestApplyScopeDisablesExcludedBackgroundServices(t *testing.T) {
	enabled := true
	cfg := config.Default()
	cfg.C2.Enabled = &enabled
	cfg.MCP.Enabled = true
	cfg.Server.CORSAllowedOrigins = []string{"https://remote.example"}
	cfg.Robots.Wechat.Enabled = true
	cfg.Robots.Wechat.BotToken = "secret"

	ApplyScope(cfg)

	if cfg.C2.EnabledEffective() {
		t.Fatal("C2 remained enabled in desktop scope")
	}
	if cfg.MCP.Enabled {
		t.Fatal("remote MCP listener remained enabled in desktop scope")
	}
	if len(cfg.Server.CORSAllowedOrigins) != 0 {
		t.Fatalf("remote CORS origins remained configured: %#v", cfg.Server.CORSAllowedOrigins)
	}
	if cfg.Robots.Wechat.Enabled || cfg.Robots.Wechat.BotToken != "" {
		t.Fatalf("robot configuration remained active: %#v", cfg.Robots.Wechat)
	}
}
