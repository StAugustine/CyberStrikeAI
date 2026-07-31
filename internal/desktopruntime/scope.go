package desktopruntime

import "cyberstrike-ai/internal/config"

// ApplyScope disables background services that are explicitly outside the
// desktop product boundary. Route and UI removal remains a separate D5 gate.
func ApplyScope(cfg *config.Config) {
	if cfg == nil {
		return
	}
	disabled := false
	cfg.C2.Enabled = &disabled
	cfg.Robots = config.RobotsConfig{}
	cfg.MCP.Enabled = false
	cfg.Server.CORSAllowedOrigins = nil
}
