package desktopruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cyberstrike-ai/internal/config"
)

func TestResolvePathsRequiresAbsoluteRoots(t *testing.T) {
	absolute := t.TempDir()
	tests := []struct {
		name  string
		roots Roots
	}{
		{name: "data", roots: Roots{DataDir: "data", ConfigDir: absolute, CacheDir: absolute, LogDir: absolute, TempDir: absolute}},
		{name: "config", roots: Roots{DataDir: absolute, ConfigDir: "config", CacheDir: absolute, LogDir: absolute, TempDir: absolute}},
		{name: "cache", roots: Roots{DataDir: absolute, ConfigDir: absolute, CacheDir: "cache", LogDir: absolute, TempDir: absolute}},
		{name: "log", roots: Roots{DataDir: absolute, ConfigDir: absolute, CacheDir: absolute, LogDir: "log", TempDir: absolute}},
		{name: "temp", roots: Roots{DataDir: absolute, ConfigDir: absolute, CacheDir: absolute, LogDir: absolute, TempDir: "temp"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolvePaths(test.roots)
			if err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("ResolvePaths() error = %v, want %s root error", err, test.name)
			}
		})
	}
}

func TestResolveAndPreparePathsAreIndependentFromCWD(t *testing.T) {
	root := t.TempDir()
	roots := Roots{
		DataDir:   filepath.Join(root, "data-root"),
		ConfigDir: filepath.Join(root, "config-root"),
		CacheDir:  filepath.Join(root, "cache-root"),
		LogDir:    filepath.Join(root, "log-root"),
		TempDir:   filepath.Join(root, "temp-root"),
	}

	paths, err := ResolvePaths(roots)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if paths.UploadsDir != filepath.Join(roots.DataDir, "chat_uploads") {
		t.Fatalf("desktop uploads directory = %q", paths.UploadsDir)
	}
	t.Chdir(t.TempDir())
	if err := paths.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for _, path := range []string{
		paths.ConfigDir,
		filepath.Dir(paths.DatabaseFile),
		paths.ResourcesDir,
		paths.UploadsDir,
		paths.WorkspaceDir,
		paths.AgentCheckpointDir,
		paths.WorkflowCheckpointDir,
		paths.ReductionDir,
		paths.LogDir,
		paths.TempDir,
	} {
		if !filepath.IsAbs(path) {
			t.Fatalf("path is not absolute: %q", path)
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("prepared path %q: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestPrepareRejectsFileWhereDirectoryIsRequired(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config-file")
	if err := os.WriteFile(configPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := ResolvePaths(Roots{
		DataDir:   filepath.Join(root, "data"),
		ConfigDir: configPath,
		CacheDir:  filepath.Join(root, "cache"),
		LogDir:    filepath.Join(root, "logs"),
		TempDir:   filepath.Join(root, "temp"),
	})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := paths.Prepare(); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("Prepare() error = %v, want config directory error", err)
	}
}

func TestApplyConfigPathsResolvesDesktopRuntimeLocations(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(Roots{
		DataDir:   filepath.Join(root, "data"),
		ConfigDir: filepath.Join(root, "config"),
		CacheDir:  filepath.Join(root, "cache"),
		LogDir:    filepath.Join(root, "logs"),
		TempDir:   filepath.Join(root, "temp"),
	})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	cfg := config.Default()
	cfg.RolesDir = "roles"
	cfg.SkillsDir = "skills"
	cfg.AgentsDir = "agents"
	cfg.Agent.SystemPromptPath = "prompts/system.md"
	cfg.MultiAgent.EinoMiddleware.ReductionRootDir = "tmp/reduction"

	if err := paths.ApplyConfigPaths(cfg); err != nil {
		t.Fatalf("ApplyConfigPaths: %v", err)
	}

	want := map[string]string{
		"database":           paths.DatabaseFile,
		"knowledge database": paths.KnowledgeDatabaseFile,
		"tools":              filepath.Join(paths.ResourcesDir, "tools"),
		"roles":              filepath.Join(paths.ResourcesDir, "roles"),
		"skills":             filepath.Join(paths.ResourcesDir, "skills"),
		"agents":             filepath.Join(paths.ResourcesDir, "agents"),
		"knowledge":          filepath.Join(paths.ResourcesDir, "knowledge_base"),
		"workspace":          paths.WorkspaceDir,
		"checkpoint":         paths.AgentCheckpointDir,
		"reduction":          paths.ReductionDir,
		"log":                paths.LogFile,
		"system prompt":      filepath.Join(paths.ConfigDir, "prompts", "system.md"),
	}
	got := map[string]string{
		"database":           cfg.Database.Path,
		"knowledge database": cfg.Database.KnowledgeDBPath,
		"tools":              cfg.Security.ToolsDir,
		"roles":              cfg.RolesDir,
		"skills":             cfg.SkillsDir,
		"agents":             cfg.AgentsDir,
		"knowledge":          cfg.Knowledge.BasePath,
		"workspace":          cfg.Agent.WorkspaceRootDir,
		"checkpoint":         cfg.MultiAgent.EinoMiddleware.CheckpointDir,
		"reduction":          cfg.MultiAgent.EinoMiddleware.ReductionRootDir,
		"log":                cfg.Log.Output,
		"system prompt":      cfg.Agent.SystemPromptPath,
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("%s path = %q, want %q", name, got[name], expected)
		}
		if !filepath.IsAbs(got[name]) {
			t.Errorf("%s path is not absolute: %q", name, got[name])
		}
	}
}

func TestApplyConfigPathsPreservesAbsoluteOverrides(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(Roots{
		DataDir:   filepath.Join(root, "data"),
		ConfigDir: filepath.Join(root, "config"),
		CacheDir:  filepath.Join(root, "cache"),
		LogDir:    filepath.Join(root, "logs"),
		TempDir:   filepath.Join(root, "temp"),
	})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	override := filepath.Join(root, "custom", "database.db")
	cfg := config.Default()
	cfg.Database.Path = override

	if err := paths.ApplyConfigPaths(cfg); err != nil {
		t.Fatalf("ApplyConfigPaths: %v", err)
	}
	if cfg.Database.Path != override {
		t.Fatalf("database override = %q, want %q", cfg.Database.Path, override)
	}
}
