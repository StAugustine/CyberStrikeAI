package desktopmigration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/desktopcredentials"
	"gopkg.in/yaml.v3"
)

func TestPrepareLegacyImportBuildsPrivateVerifiedSnapshotWithoutChangingSource(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	sourceRoot := t.TempDir()
	writeLegacyImportTestConfig(t, sourceRoot, "v1.7.8", "data/conversations.db")
	writeBackupTestFile(t, filepath.Join(sourceRoot, "tools", "custom.yaml"), "name: custom\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "roles", "operator.yaml"), "name: operator\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "skills", "review", "SKILL.md"), "# Review\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "agents", "helper.md"), "# Helper\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "knowledge_base", "notes.md"), "legacy notes\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "chat_uploads", "proof.txt"), "legacy proof\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "data", "eino-checkpoints", "run.ckpt"), "agent checkpoint\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "data", "workflow-checkpoints", "workflow.json"), "{}\n", 0o600)
	writeBackupTestFile(t, filepath.Join(sourceRoot, "data", "conversation_artifacts", "conversation", "trace.txt"), "trace\n", 0o600)
	databasePath := filepath.Join(sourceRoot, "data", "conversations.db")
	database := openBackupTestWALDatabase(t, databasePath, "legacy conversation")
	defer database.Close()
	prepareLegacyImportRBAC(t, database)
	knowledgePath := filepath.Join(sourceRoot, "data", "knowledge.db")
	knowledge := openBackupTestWALDatabase(t, knowledgePath, "legacy knowledge")
	defer knowledge.Close()

	sourceFiles := []string{
		filepath.Join(sourceRoot, "config.yaml"),
		databasePath,
		databasePath + "-wal",
		knowledgePath,
		knowledgePath + "-wal",
	}
	before := readLegacyImportTestFiles(t, sourceFiles)
	preparedAt := time.Date(2026, 7, 31, 20, 30, 0, 0, time.UTC)
	session, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", preparedAt)
	if err != nil {
		t.Fatalf("PrepareLegacyImport: %v", err)
	}
	report := session.Report()
	if report.SourceName != filepath.Base(sourceRoot) || report.SourceVersion != "v1.7.8" || report.TargetVersion != "0.1.0" {
		t.Fatalf("legacy import report identity = %#v", report)
	}
	if report.RemovedUserAccounts != 1 {
		t.Fatalf("legacy import removed users = %d", report.RemovedUserAccounts)
	}
	if len(report.PlaintextCredentialPaths) != 1 || report.PlaintextCredentialPaths[0] != desktopcredentials.PathFOFA {
		t.Fatalf("legacy import credential report = %#v", report.PlaintextCredentialPaths)
	}
	for _, capability := range []string{"c2", "local_terminal", "multi_user_rbac", "remote_service", "robots", "webshell"} {
		if !containsImportTestString(report.ExcludedCapabilities, capability) {
			t.Fatalf("legacy import excluded capabilities = %v", report.ExcludedCapabilities)
		}
	}
	for _, field := range []string{"log.output", "mcp.auth_header", "mcp.auth_header_value"} {
		if !containsImportTestString(report.IgnoredConfigPaths, field) {
			t.Fatalf("legacy import ignored config paths = %v", report.IgnoredConfigPaths)
		}
	}

	backupDirectory := filepath.Join(paths.BackupsDir, session.State().BackupID)
	manifest, err := VerifyBackup(backupDirectory)
	if err != nil {
		t.Fatalf("VerifyBackup(import): %v", err)
	}
	if manifest.Kind != "import" || manifest.FromVersion != "v1.7.8" || manifest.ToVersion != "0.1.0" {
		t.Fatalf("legacy import manifest = %#v", manifest)
	}
	manifestPaths := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		manifestPaths[file.Path] = true
	}
	for _, path := range []string{
		"config/config.yaml",
		"data/databases/conversations.db",
		"data/databases/knowledge.db",
		"data/databases/conversation_artifacts/conversation/trace.txt",
		"data/resources/tools/custom.yaml",
		"data/resources/roles/operator.yaml",
		"data/resources/skills/review/SKILL.md",
		"data/resources/agents/helper.md",
		"data/resources/knowledge_base/notes.md",
		"data/chat_uploads/proof.txt",
		"data/checkpoints/agents/run.ckpt",
		"data/checkpoints/workflows/workflow.json",
	} {
		if !manifestPaths[path] {
			t.Errorf("legacy import manifest missing %q", path)
		}
	}
	assertBackupTestSQLiteValue(t, filepath.Join(backupDirectory, "payload", "data", "databases", "conversations.db"), "legacy conversation")
	assertBackupTestSQLiteValue(t, filepath.Join(backupDirectory, "payload", "data", "databases", "knowledge.db"), "legacy knowledge")
	if users := countLegacyImportUsers(t, filepath.Join(backupDirectory, "payload", "data", "databases", "conversations.db")); users != 1 {
		t.Fatalf("normalized imported users = %d, want only local admin", users)
	}
	var importedConfig config.Config
	configData, err := os.ReadFile(filepath.Join(backupDirectory, "payload", "config", "config.yaml"))
	if err != nil {
		t.Fatalf("read imported config: %v", err)
	}
	if err := yaml.Unmarshal(configData, &importedConfig); err != nil {
		t.Fatalf("parse imported config: %v", err)
	}
	if importedConfig.Server.Host != "127.0.0.1" || importedConfig.Server.Port != 0 || importedConfig.Server.TLSEnabled || importedConfig.MCP.Enabled || importedConfig.MCP.AuthHeaderValue != "" || importedConfig.MCP.AllowGlobalAccess {
		t.Fatalf("imported desktop service scope = %#v, MCP=%#v", importedConfig.Server, importedConfig.MCP)
	}
	if importedConfig.C2.Enabled == nil || *importedConfig.C2.Enabled || importedConfig.Database.Path != "data/conversations.db" || importedConfig.Database.KnowledgeDBPath != "data/knowledge.db" {
		t.Fatalf("imported desktop config paths/scope = %#v", importedConfig)
	}
	if importedConfig.Security.ToolsDir != "tools" || importedConfig.RolesDir != "roles" || importedConfig.SkillsDir != "skills" || importedConfig.AgentsDir != "agents" || importedConfig.Knowledge.BasePath != "knowledge_base" {
		t.Fatalf("imported resource paths were not canonicalized: %#v", importedConfig)
	}

	stateData, err := os.ReadFile(paths.ImportStateFile)
	if err != nil {
		t.Fatalf("read desktop import state: %v", err)
	}
	if strings.Contains(string(stateData), "legacy-fofa-secret") || strings.Contains(string(stateData), "legacy-mcp-secret") || strings.Contains(string(stateData), sourceRoot) {
		t.Fatal("desktop import state exposed a secret or absolute source path")
	}
	stateInfo, err := os.Stat(paths.ImportStateFile)
	if err != nil || !isPrivateTestFileMode(stateInfo.Mode()) {
		t.Fatalf("desktop import state permissions = %v, err=%v", stateInfo, err)
	}
	loaded, exists, err := LoadImportState(paths.ImportStateFile)
	if err != nil || !exists || loaded.BackupID != manifest.ID {
		t.Fatalf("LoadImportState = %#v, %t, %v", loaded, exists, err)
	}
	summaries, err := ListBackups(paths)
	if err != nil || len(summaries) != 1 || !summaries[0].Protected || summaries[0].Deletable {
		t.Fatalf("pending import backup catalog = %#v, %v", summaries, err)
	}
	if _, err := os.Lstat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("legacy import preflight changed live config: %v", err)
	}
	for path, content := range before {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(content) {
			t.Fatalf("legacy import source changed at %q: err=%v", path, err)
		}
	}
	if users := countLegacyImportUsersOnDB(t, database); users != 2 {
		t.Fatalf("legacy import changed source users: %d", users)
	}
}

func TestInspectLegacyImportSourceVersionBoundary(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	for _, version := range []string{"v1.7.8", "1.7.9", "v1.7.99"} {
		t.Run("supported-"+version, func(t *testing.T) {
			sourceRoot := t.TempDir()
			writeLegacyImportTestConfig(t, sourceRoot, version, "data/conversations.db")
			if _, err := inspectLegacyImportSource(sourceRoot, paths); err != nil {
				t.Fatalf("inspectLegacyImportSource(%s): %v", version, err)
			}
		})
	}
	for _, version := range []string{"v1.7.7", "v1.8.0", "not-a-version"} {
		t.Run("unsupported-"+version, func(t *testing.T) {
			sourceRoot := t.TempDir()
			writeLegacyImportTestConfig(t, sourceRoot, version, "data/conversations.db")
			if _, err := inspectLegacyImportSource(sourceRoot, paths); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("inspectLegacyImportSource(%s) error = %v", version, err)
			}
		})
	}
}

func TestPrepareLegacyImportRejectsAbsoluteAndSymlinkedManagedPaths(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	t.Run("absolute database", func(t *testing.T) {
		sourceRoot := t.TempDir()
		writeLegacyImportTestConfig(t, sourceRoot, "v1.7.9", filepath.Join(sourceRoot, "data", "conversations.db"))
		if _, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", time.Now()); err == nil || !strings.Contains(err.Error(), "relative path") {
			t.Fatalf("PrepareLegacyImport absolute path error = %v", err)
		}
		assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	})
	t.Run("symlinked tools", func(t *testing.T) {
		sourceRoot := t.TempDir()
		writeLegacyImportTestConfig(t, sourceRoot, "v1.7.9", "data/conversations.db")
		external := t.TempDir()
		writeBackupTestFile(t, filepath.Join(external, "tool.yaml"), "name: outside\n", 0o600)
		if err := os.Symlink(external, filepath.Join(sourceRoot, "tools")); err != nil {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
		if _, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", time.Now()); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("PrepareLegacyImport symlink error = %v", err)
		}
		assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	})
}

func TestCommitLegacyImportOfflineCreatesRollbackAndCanRestorePreviousDesktop(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: current\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "current.md"), "current role\n", 0o600)
	currentDatabase := openBackupTestWALDatabase(t, paths.DatabaseFile, "current desktop")
	if err := currentDatabase.Close(); err != nil {
		t.Fatalf("close current desktop database: %v", err)
	}

	sourceRoot := prepareMinimalLegacyImportSource(t, "v1.7.9", "imported desktop")
	session, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", time.Date(2026, 7, 31, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PrepareLegacyImport: %v", err)
	}
	resumed, exists, err := LoadPendingImportSession(paths)
	if err != nil || !exists {
		t.Fatalf("LoadPendingImportSession exists=%t err=%v", exists, err)
	}
	result, err := resumed.CommitOffline(context.Background(), time.Date(2026, 7, 31, 21, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CommitOffline: %v", err)
	}
	if result.BackupID != session.State().BackupID || result.RollbackBackupID == "" || result.SourceVersion != "v1.7.9" || result.TargetVersion != "0.1.0" {
		t.Fatalf("legacy import commit result = %#v", result)
	}
	if _, exists, err := LoadImportState(paths.ImportStateFile); err != nil || exists {
		t.Fatalf("committed import state exists=%t err=%v", exists, err)
	}
	assertBackupTestSQLiteValue(t, paths.DatabaseFile, "imported desktop")
	importedConfig, err := os.ReadFile(paths.ConfigFile)
	if err != nil || !strings.Contains(string(importedConfig), "version: v1.7.9") {
		t.Fatalf("committed import config = %q, err=%v", importedConfig, err)
	}
	if _, err := VerifyBackup(filepath.Join(paths.BackupsDir, result.BackupID)); err != nil {
		t.Fatalf("committed import recovery point: %v", err)
	}
	if _, err := VerifyBackup(filepath.Join(paths.BackupsDir, result.RollbackBackupID)); err != nil {
		t.Fatalf("pre-import recovery point: %v", err)
	}
	if _, err := RestoreBackup(context.Background(), paths, result.RollbackBackupID); err != nil {
		t.Fatalf("restore pre-import desktop: %v", err)
	}
	assertBackupTestSQLiteValue(t, paths.DatabaseFile, "current desktop")
	assertBackupTestFileContent(t, paths.ConfigFile, "version: current\n")
	assertBackupTestFileContent(t, filepath.Join(paths.ResourcesDir, "roles", "current.md"), "current role\n")
}

func TestCommitLegacyImportRejectsTamperedSnapshotBeforeChangingLiveData(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: current\n", 0o600)
	sourceRoot := prepareMinimalLegacyImportSource(t, "v1.7.9", "imported desktop")
	session, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", time.Now())
	if err != nil {
		t.Fatalf("PrepareLegacyImport: %v", err)
	}
	importedConfig := filepath.Join(paths.BackupsDir, session.State().BackupID, "payload", "config", "config.yaml")
	if err := os.WriteFile(importedConfig, []byte("tampered\n"), 0o600); err != nil {
		t.Fatalf("tamper import snapshot: %v", err)
	}
	result, err := session.CommitOffline(context.Background(), time.Now().Add(time.Second))
	if err == nil || result.RollbackBackupID != "" {
		t.Fatalf("CommitOffline tampered result = %#v, err=%v", result, err)
	}
	assertBackupTestFileContent(t, paths.ConfigFile, "version: current\n")
	if _, exists, err := LoadImportState(paths.ImportStateFile); err != nil || !exists {
		t.Fatalf("failed import state exists=%t err=%v", exists, err)
	}
	entries, err := os.ReadDir(paths.BackupsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("tampered import created a rollback backup: entries=%v err=%v", entries, err)
	}
}

func TestCancelLegacyImportRemovesOnlyPendingSnapshot(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: current\n", 0o600)
	retained, err := CreateUpgradeBackup(context.Background(), paths, "0.1.0", "0.1.0", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	sourceRoot := prepareMinimalLegacyImportSource(t, "v1.7.9", "imported desktop")
	session, err := PrepareLegacyImport(context.Background(), paths, sourceRoot, "0.1.0", time.Now())
	if err != nil {
		t.Fatalf("PrepareLegacyImport: %v", err)
	}
	if err := session.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := session.Cancel(); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if _, exists, err := LoadImportState(paths.ImportStateFile); err != nil || exists {
		t.Fatalf("cancelled import state exists=%t err=%v", exists, err)
	}
	if _, err := os.Lstat(filepath.Join(paths.BackupsDir, session.State().BackupID)); !os.IsNotExist(err) {
		t.Fatalf("cancelled import snapshot remains: %v", err)
	}
	if _, err := VerifyBackup(retained.Directory); err != nil {
		t.Fatalf("Cancel changed unrelated backup: %v", err)
	}
	assertBackupTestFileContent(t, paths.ConfigFile, "version: current\n")
}

func writeLegacyImportTestConfig(t *testing.T, root, version, databasePath string) {
	t.Helper()
	content := "version: " + version + "\n" +
		"server:\n  host: 0.0.0.0\n  port: 8080\n  tls_enabled: true\n" +
		"log:\n  level: info\n  output: legacy.log\n" +
		"mcp:\n  enabled: true\n  host: 0.0.0.0\n  port: 8081\n  auth_header: X-Legacy-Token\n  auth_header_value: legacy-mcp-secret\n  allow_global_access: true\n" +
		"fofa:\n  api_key: legacy-fofa-secret\n" +
		"agent:\n  workspace_root_dir: data/workspaces\n" +
		"multi_agent:\n  eino_middleware:\n    checkpoint_dir: data/eino-checkpoints\n" +
		"security:\n  tools_dir: tools\n" +
		"database:\n  path: " + databasePath + "\n  knowledge_db_path: data/knowledge.db\n" +
		"knowledge:\n  base_path: knowledge_base\n" +
		"roles_dir: roles\nskills_dir: skills\nagents_dir: agents\n" +
		"c2:\n  enabled: true\n"
	writeBackupTestFile(t, filepath.Join(root, "config.yaml"), content, 0o600)
}

func prepareMinimalLegacyImportSource(t *testing.T, version, value string) string {
	t.Helper()
	root := t.TempDir()
	writeLegacyImportTestConfig(t, root, version, "data/conversations.db")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatalf("prepare legacy source data directory: %v", err)
	}
	conversation := openBackupTestWALDatabase(t, filepath.Join(root, "data", "conversations.db"), value)
	if err := conversation.Close(); err != nil {
		t.Fatalf("close legacy conversation source: %v", err)
	}
	knowledge := openBackupTestWALDatabase(t, filepath.Join(root, "data", "knowledge.db"), "legacy knowledge")
	if err := knowledge.Close(); err != nil {
		t.Fatalf("close legacy knowledge source: %v", err)
	}
	return root
}

func prepareLegacyImportRBAC(t *testing.T, database *sql.DB) {
	t.Helper()
	statements := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE rbac_roles (id TEXT PRIMARY KEY, is_system INTEGER NOT NULL)`,
		`CREATE TABLE rbac_users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, is_builtin INTEGER NOT NULL)`,
		`CREATE TABLE rbac_user_roles (user_id TEXT NOT NULL, role_id TEXT NOT NULL, FOREIGN KEY(user_id) REFERENCES rbac_users(id) ON DELETE CASCADE, FOREIGN KEY(role_id) REFERENCES rbac_roles(id) ON DELETE CASCADE)`,
		`INSERT INTO rbac_roles(id, is_system) VALUES ('admin', 1), ('custom', 0)`,
		`INSERT INTO rbac_users(id, username, is_builtin) VALUES ('admin', 'admin', 1), ('user-1', 'operator', 0)`,
		`INSERT INTO rbac_user_roles(user_id, role_id) VALUES ('admin', 'admin'), ('user-1', 'custom')`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("prepare legacy import RBAC: %v", err)
		}
	}
}

func countLegacyImportUsers(t *testing.T, path string) int {
	t.Helper()
	database, err := sql.Open("sqlite3", path+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open imported users database: %v", err)
	}
	defer database.Close()
	return countLegacyImportUsersOnDB(t, database)
}

func countLegacyImportUsersOnDB(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM rbac_users").Scan(&count); err != nil {
		t.Fatalf("count legacy import users: %v", err)
	}
	return count
}

func readLegacyImportTestFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read legacy import source %q: %v", path, err)
		}
		result[path] = data
	}
	return result
}

func containsImportTestString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestImportStateNeverSerializesSourcePathsOrSecrets(t *testing.T) {
	state := ImportState{
		SchemaVersion: ImportStateSchemaVersion,
		Status:        "pending",
		BackupID:      "import-20260731T203000.000000000Z",
		PreparedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Report: ImportReport{
			SchemaVersion: ImportReportSchemaVersion,
			SourceName:    "legacy-instance",
			SourceVersion: "v1.7.9",
			TargetVersion: "0.1.0",
			BackupID:      "import-20260731T203000.000000000Z",
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/Users/") || strings.Contains(string(data), "api-key") {
		t.Fatalf("import state exposed sensitive source details: %s", data)
	}
}
