package desktopruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cyberstrike-ai/internal/config"
)

const privateDirectoryMode = 0o700

// Roots are supplied by the desktop shell from the platform-specific Tauri
// path resolver. The Go core never derives desktop storage from its CWD.
type Roots struct {
	DataDir   string
	ConfigDir string
	CacheDir  string
	LogDir    string
	TempDir   string
}

// Paths contains every writable location owned by the desktop core.
type Paths struct {
	Roots
	ConfigFile            string
	DatabaseFile          string
	KnowledgeDatabaseFile string
	ResourcesDir          string
	ResourceStateFile     string
	UpgradeStateFile      string
	UploadsDir            string
	WorkspaceDir          string
	AgentCheckpointDir    string
	WorkflowCheckpointDir string
	ReductionDir          string
	BackupsDir            string
	LogFile               string
}

// ResolvePaths validates absolute platform roots and derives fixed desktop
// locations without touching the filesystem.
func ResolvePaths(roots Roots) (Paths, error) {
	resolved := Roots{}
	values := []struct {
		name  string
		value string
		dest  *string
	}{
		{name: "data", value: roots.DataDir, dest: &resolved.DataDir},
		{name: "config", value: roots.ConfigDir, dest: &resolved.ConfigDir},
		{name: "cache", value: roots.CacheDir, dest: &resolved.CacheDir},
		{name: "log", value: roots.LogDir, dest: &resolved.LogDir},
		{name: "temp", value: roots.TempDir, dest: &resolved.TempDir},
	}
	for _, item := range values {
		value := strings.TrimSpace(item.value)
		if value == "" {
			return Paths{}, fmt.Errorf("desktop %s directory is required", item.name)
		}
		if !filepath.IsAbs(value) {
			return Paths{}, fmt.Errorf("desktop %s directory must be absolute: %q", item.name, value)
		}
		*item.dest = filepath.Clean(value)
	}

	resourcesDir := filepath.Join(resolved.DataDir, "resources")
	return Paths{
		Roots:                 resolved,
		ConfigFile:            filepath.Join(resolved.ConfigDir, "config.yaml"),
		DatabaseFile:          filepath.Join(resolved.DataDir, "databases", "conversations.db"),
		KnowledgeDatabaseFile: filepath.Join(resolved.DataDir, "databases", "knowledge.db"),
		ResourcesDir:          resourcesDir,
		ResourceStateFile:     filepath.Join(resolved.DataDir, "resource-state.json"),
		UpgradeStateFile:      filepath.Join(resolved.DataDir, "upgrade-state.json"),
		UploadsDir:            filepath.Join(resolved.DataDir, "chat_uploads"),
		WorkspaceDir:          filepath.Join(resolved.DataDir, "workspaces"),
		AgentCheckpointDir:    filepath.Join(resolved.DataDir, "checkpoints", "agents"),
		WorkflowCheckpointDir: filepath.Join(resolved.DataDir, "checkpoints", "workflows"),
		ReductionDir:          filepath.Join(resolved.CacheDir, "reduction"),
		BackupsDir:            filepath.Join(resolved.DataDir, "backups"),
		LogFile:               filepath.Join(resolved.LogDir, "cyberstrike-ai.log"),
	}, nil
}

// Prepare creates and verifies every writable directory before the core opens
// configuration, logs, or databases.
func (p Paths) Prepare() error {
	directories := []struct {
		name string
		path string
	}{
		{name: "data", path: p.DataDir},
		{name: "config", path: p.ConfigDir},
		{name: "cache", path: p.CacheDir},
		{name: "log", path: p.LogDir},
		{name: "temp", path: p.TempDir},
		{name: "database", path: filepath.Dir(p.DatabaseFile)},
		{name: "resources", path: p.ResourcesDir},
		{name: "uploads", path: p.UploadsDir},
		{name: "workspace", path: p.WorkspaceDir},
		{name: "agent checkpoints", path: p.AgentCheckpointDir},
		{name: "workflow checkpoints", path: p.WorkflowCheckpointDir},
		{name: "reduction cache", path: p.ReductionDir},
		{name: "backups", path: p.BackupsDir},
	}
	for _, directory := range directories {
		if err := prepareWritableDirectory(directory.path); err != nil {
			return fmt.Errorf("prepare desktop %s directory: %w", directory.name, err)
		}
	}
	return nil
}

func prepareWritableDirectory(path string) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("directory must be absolute: %q", path)
	}
	if err := os.MkdirAll(path, privateDirectoryMode); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	probe, err := os.CreateTemp(path, ".desktop-write-check-*")
	if err != nil {
		return fmt.Errorf("directory is not writable: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close write probe: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove write probe: %w", err)
	}
	return nil
}

// ApplyConfigPaths makes every runtime path used by the desktop core
// independent from the process CWD. Absolute user overrides remain unchanged.
func (p Paths) ApplyConfigPaths(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("desktop config is required")
	}

	cfg.Database.Path = resolveLegacyPath(p.DataDir, cfg.Database.Path, p.DatabaseFile, "data")
	cfg.Database.KnowledgeDBPath = resolveLegacyPath(p.DataDir, cfg.Database.KnowledgeDBPath, p.KnowledgeDatabaseFile, "data")
	cfg.Security.ToolsDir = resolveResourcePath(p.ResourcesDir, cfg.Security.ToolsDir, "tools")
	cfg.RolesDir = resolveResourcePath(p.ResourcesDir, cfg.RolesDir, "roles")
	cfg.SkillsDir = resolveResourcePath(p.ResourcesDir, cfg.SkillsDir, "skills")
	cfg.AgentsDir = resolveResourcePath(p.ResourcesDir, cfg.AgentsDir, "agents")
	cfg.Knowledge.BasePath = resolveResourcePath(p.ResourcesDir, cfg.Knowledge.BasePath, "knowledge_base")
	cfg.Agent.WorkspaceRootDir = resolveLegacyPath(p.DataDir, cfg.Agent.WorkspaceRootDir, p.WorkspaceDir, "data")
	cfg.MultiAgent.EinoMiddleware.CheckpointDir = resolveLegacyPath(p.DataDir, cfg.MultiAgent.EinoMiddleware.CheckpointDir, p.AgentCheckpointDir, "data")
	cfg.MultiAgent.EinoMiddleware.ReductionRootDir = resolveLegacyPath(p.CacheDir, cfg.MultiAgent.EinoMiddleware.ReductionRootDir, p.ReductionDir, "tmp")
	cfg.Agent.SystemPromptPath = resolveOptionalPath(p.ConfigDir, cfg.Agent.SystemPromptPath)
	cfg.Server.TLSCertPath = resolveOptionalPath(p.ConfigDir, cfg.Server.TLSCertPath)
	cfg.Server.TLSKeyPath = resolveOptionalPath(p.ConfigDir, cfg.Server.TLSKeyPath)

	logOutput := strings.TrimSpace(cfg.Log.Output)
	if logOutput == "" || logOutput == "stdout" || logOutput == "stderr" {
		cfg.Log.Output = p.LogFile
	} else {
		cfg.Log.Output = resolveOptionalPath(p.LogDir, logOutput)
	}
	return nil
}

func resolveResourcePath(resourcesDir, value, fallbackName string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return filepath.Join(resourcesDir, fallbackName)
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(resourcesDir, filepath.Clean(value))
}

func resolveLegacyPath(baseDir, value, fallback, legacyPrefix string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	cleaned := filepath.Clean(value)
	if cleaned == filepath.Join(legacyPrefix, filepath.Base(fallback)) {
		return fallback
	}
	prefix := legacyPrefix + string(filepath.Separator)
	if strings.HasPrefix(cleaned, prefix) {
		cleaned = strings.TrimPrefix(cleaned, prefix)
	}
	return filepath.Join(baseDir, cleaned)
}

func resolveOptionalPath(baseDir, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(baseDir, filepath.Clean(value))
}
