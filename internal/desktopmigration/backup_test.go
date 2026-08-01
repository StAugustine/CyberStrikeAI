package desktopmigration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
)

func TestCreateUpgradeBackupIncludesCommittedWALAndVerifiesPayload(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "desktop: true\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "custom.md"), "custom role", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.UploadsDir, "notes", "proof.txt"), "desktop proof", 0o600)
	writeBackupTestFile(t, paths.ResourceStateFile, `{"schema_version":1,"app_version":"1.0.0","files":{}}`, 0o600)

	conversationDB := openBackupTestWALDatabase(t, paths.DatabaseFile, "conversation-from-wal")
	defer conversationDB.Close()
	knowledgeDB := openBackupTestWALDatabase(t, paths.KnowledgeDatabaseFile, "knowledge-from-wal")
	defer knowledgeDB.Close()

	createdAt := time.Date(2026, time.July, 31, 18, 30, 0, 123456789, time.FixedZone("test", -7*60*60))
	result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", createdAt)
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	if filepath.Dir(result.Directory) != paths.BackupsDir {
		t.Fatalf("backup directory = %q, want child of %q", result.Directory, paths.BackupsDir)
	}
	if result.Manifest.FromVersion != "1.0.0" || result.Manifest.ToVersion != "1.1.0" {
		t.Fatalf("backup manifest versions = %q -> %q", result.Manifest.FromVersion, result.Manifest.ToVersion)
	}
	if result.Manifest.CreatedAt != createdAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("backup manifest created_at = %q", result.Manifest.CreatedAt)
	}
	if result.Manifest.TotalBytes <= 0 {
		t.Fatalf("backup manifest total bytes = %d", result.Manifest.TotalBytes)
	}

	files := make(map[string]BackupFile, len(result.Manifest.Files))
	for _, file := range result.Manifest.Files {
		files[file.Path] = file
	}
	for _, path := range []string{
		"config/config.yaml",
		"data/resource-state.json",
		"data/resources/roles/custom.md",
		"data/chat_uploads/notes/proof.txt",
		"data/databases/conversations.db",
		"data/databases/knowledge.db",
	} {
		if _, ok := files[path]; !ok {
			t.Errorf("backup manifest missing %q: %#v", path, result.Manifest.Files)
		}
	}
	for _, path := range []string{"data/databases/conversations.db", "data/databases/knowledge.db"} {
		if files[path].Kind != "sqlite" {
			t.Errorf("backup manifest kind for %q = %q", path, files[path].Kind)
		}
		info, err := os.Stat(filepath.Join(result.Directory, "payload", filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("inspect backup SQLite permissions for %q: %v", path, err)
		} else if !isPrivateTestFileMode(info.Mode()) {
			t.Errorf("backup SQLite permissions for %q = %v", path, info.Mode().Perm())
		}
	}

	verified, err := VerifyBackup(result.Directory)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if verified.ID != result.Manifest.ID {
		t.Fatalf("verified backup id = %q, want %q", verified.ID, result.Manifest.ID)
	}
	assertBackupTestSQLiteValue(t, filepath.Join(result.Directory, "payload", "data", "databases", "conversations.db"), "conversation-from-wal")
	assertBackupTestSQLiteValue(t, filepath.Join(result.Directory, "payload", "data", "databases", "knowledge.db"), "knowledge-from-wal")
	assertBackupTestSQLiteValue(t, paths.DatabaseFile, "conversation-from-wal")
	assertBackupTestSQLiteValue(t, paths.KnowledgeDatabaseFile, "knowledge-from-wal")

	entries, err := os.ReadDir(paths.BackupsDir)
	if err != nil {
		t.Fatalf("read backup directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(result.Directory) || strings.Contains(entries[0].Name(), ".pending") {
		t.Fatalf("published backup entries = %#v", entries)
	}
	if _, err := os.Stat(filepath.Join(result.Directory, "payload", "data", "databases", "conversations.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("backup unexpectedly contains SQLite WAL: %v", err)
	}
}

func isPrivateTestFileMode(mode os.FileMode) bool {
	if !mode.IsRegular() {
		return false
	}
	// Go's FileMode does not expose Windows ACLs. Files under the per-user
	// application directory inherit its ACL and appear as 0666 to Go.
	if runtime.GOOS == "windows" {
		return mode.Perm()&0o111 == 0
	}
	return mode.Perm()&0o077 == 0
}

func TestCreateUpgradeBackupRejectsSymlinkAndRemovesStaging(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	outside := filepath.Join(filepath.Dir(paths.DataDir), "outside.txt")
	writeBackupTestFile(t, outside, "outside sentinel", 0o600)
	link := filepath.Join(paths.DataDir, "unsafe-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	_, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CreateUpgradeBackup error = %v, want symbolic link rejection", err)
	}
	assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "outside sentinel" {
		t.Fatalf("outside source changed: content=%q err=%v", content, err)
	}
}

func TestCreateUpgradeBackupDoesNotRecoverSourceWAL(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	livePath := filepath.Join(filepath.Dir(paths.DataDir), "live-wal.db")
	liveDatabase := openBackupTestWALDatabase(t, livePath, "crash-recovery-value")
	copyBackupTestFile(t, livePath, paths.DatabaseFile)
	copyBackupTestFile(t, livePath+"-wal", paths.DatabaseFile+"-wal")
	if err := liveDatabase.Close(); err != nil {
		t.Fatalf("close live WAL database: %v", err)
	}

	sourceHashes := map[string]string{}
	for _, path := range []string{paths.DatabaseFile, paths.DatabaseFile + "-wal"} {
		hash, err := hashFile(path)
		if err != nil {
			t.Fatalf("hash source SQLite artifact %q: %v", path, err)
		}
		sourceHashes[path] = hash
	}
	if _, err := os.Stat(paths.DatabaseFile + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("crash fixture unexpectedly has SHM file: %v", err)
	}

	result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	assertBackupTestSQLiteValue(t, filepath.Join(result.Directory, "payload", "data", "databases", "conversations.db"), "crash-recovery-value")
	for path, wantHash := range sourceHashes {
		gotHash, err := hashFile(path)
		if err != nil || gotHash != wantHash {
			t.Fatalf("source SQLite artifact %q changed: got=%q want=%q err=%v", path, gotHash, wantHash, err)
		}
	}
	if _, err := os.Stat(paths.DatabaseFile + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("backup touched source SQLite SHM state: %v", err)
	}
}

func TestCreateUpgradeBackupRejectsCorruptSQLiteAndRemovesStaging(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.DatabaseFile, "not a sqlite database", 0o600)

	_, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("CreateUpgradeBackup error = %v, want SQLite error", err)
	}
	assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	content, readErr := os.ReadFile(paths.DatabaseFile)
	if readErr != nil || string(content) != "not a sqlite database" {
		t.Fatalf("corrupt SQLite source changed: content=%q err=%v", content, readErr)
	}
}

func TestVerifyBackupDetectsPayloadTampering(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "desktop: true\n", 0o600)
	result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	configBackup := filepath.Join(result.Directory, "payload", "config", "config.yaml")
	if err := os.WriteFile(configBackup, []byte("tampered\n"), 0o600); err != nil {
		t.Fatalf("tamper backup payload: %v", err)
	}
	if _, err := VerifyBackup(result.Directory); err == nil || (!strings.Contains(err.Error(), "size mismatch") && !strings.Contains(err.Error(), "checksum mismatch")) {
		t.Fatalf("VerifyBackup error = %v, want payload integrity error", err)
	}
}

func TestVerifyBackupRejectsWindowsPathTraversal(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "desktop: true\n", 0o600)
	result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	result.Manifest.Files[0].Path = `data\..\outside.txt`
	manifestData, err := json.Marshal(result.Manifest)
	if err != nil {
		t.Fatalf("encode malicious backup manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatalf("write malicious backup manifest: %v", err)
	}
	if _, err := VerifyBackup(result.Directory); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("VerifyBackup error = %v, want Windows path traversal rejection", err)
	}
}

func TestVerifyBackupRejectsReservedRestorePaths(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "config/other.yaml", want: "unsupported config path"},
		{path: "data/backups/replace.txt", want: "reserved path"},
		{path: "data/upgrade-state.json", want: "reserved path"},
		{path: "data/restore-state.json", want: "reserved path"},
		{path: "data/import-state.json", want: "reserved path"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			paths := prepareBackupTestPaths(t)
			writeBackupTestFile(t, paths.ConfigFile, "desktop: true\n", 0o600)
			result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
			if err != nil {
				t.Fatalf("CreateUpgradeBackup: %v", err)
			}
			result.Manifest.Files[0].Path = test.path
			manifestData, err := json.Marshal(result.Manifest)
			if err != nil {
				t.Fatalf("encode reserved backup manifest: %v", err)
			}
			if err := os.WriteFile(filepath.Join(result.Directory, "manifest.json"), manifestData, 0o600); err != nil {
				t.Fatalf("write reserved backup manifest: %v", err)
			}
			if _, err := VerifyBackup(result.Directory); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyBackup error = %v, want %q rejection", err, test.want)
			}
		})
	}
}

func TestVerifyBackupRejectsPayloadDirectorySymlink(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "desktop: true\n", 0o600)
	result, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	externalConfigDir := filepath.Join(paths.TempDir, "external-config")
	writeBackupTestFile(t, filepath.Join(externalConfigDir, "config.yaml"), "desktop: true\n", 0o600)
	payloadConfigDir := filepath.Join(result.Directory, "payload", "config")
	if err := os.RemoveAll(payloadConfigDir); err != nil {
		t.Fatalf("remove backup payload directory: %v", err)
	}
	if err := os.Symlink(externalConfigDir, payloadConfigDir); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := VerifyBackup(result.Directory); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("VerifyBackup error = %v, want payload symbolic link rejection", err)
	}
}

func TestCreateUpgradeBackupHonorsCancelledContext(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateUpgradeBackup(ctx, paths, "1.0.0", "1.1.0", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateUpgradeBackup error = %v, want context cancellation", err)
	}
	assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
}

func prepareBackupTestPaths(t *testing.T) desktopruntime.Paths {
	t.Helper()
	root := t.TempDir()
	paths, err := desktopruntime.ResolvePaths(desktopruntime.Roots{
		DataDir:   filepath.Join(root, "data"),
		ConfigDir: filepath.Join(root, "config"),
		CacheDir:  filepath.Join(root, "cache"),
		LogDir:    filepath.Join(root, "logs"),
		TempDir:   filepath.Join(root, "temp"),
	})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := paths.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return paths
}

func writeBackupTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("prepare test file directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write test file %q: %v", path, err)
	}
}

func copyBackupTestFile(t *testing.T, source, destination string) {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read test source file %q: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatalf("prepare test destination directory: %v", err)
	}
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		t.Fatalf("write test destination file %q: %v", destination, err)
	}
}

func openBackupTestWALDatabase(t *testing.T, path, value string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open test SQLite database: %v", err)
	}
	for _, statement := range []string{
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE backup_test (value TEXT NOT NULL)",
		"INSERT INTO backup_test(value) VALUES (?)",
	} {
		var execErr error
		if strings.Contains(statement, "VALUES") {
			_, execErr = database.Exec(statement, value)
		} else {
			_, execErr = database.Exec(statement)
		}
		if execErr != nil {
			_ = database.Close()
			t.Fatalf("prepare test SQLite database: %v", execErr)
		}
	}
	walInfo, err := os.Stat(path + "-wal")
	if err != nil || walInfo.Size() == 0 {
		_ = database.Close()
		t.Fatalf("test SQLite WAL is unavailable: info=%v err=%v", walInfo, err)
	}
	return database
}

func assertBackupTestSQLiteValue(t *testing.T, path, want string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open SQLite snapshot %q: %v", path, err)
	}
	defer database.Close()
	var got string
	if err := database.QueryRow("SELECT value FROM backup_test").Scan(&got); err != nil {
		t.Fatalf("query SQLite snapshot %q: %v", path, err)
	}
	if got != want {
		t.Fatalf("SQLite snapshot value = %q, want %q", got, want)
	}
}

func assertBackupTestDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read backup directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("backup directory contains incomplete entries: %#v", entries)
	}
}
