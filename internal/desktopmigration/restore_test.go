package desktopmigration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
)

func TestRestoreBackupReplacesManagedDataAndKeepsRecoveryPoint(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "old role", 0o600)
	writeBackupTestFile(t, paths.ResourceStateFile, `{"schema_version":1,"app_version":"1.0.0","files":{}}`, 0o600)
	database := openBackupTestWALDatabase(t, paths.DatabaseFile, "old database value")
	backup, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close restore source database: %v", err)
	}

	writeBackupTestFile(t, paths.ConfigFile, "version: new\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "new role", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.DataDir, "new-only.txt"), "remove me", 0o600)
	writeBackupTestFile(t, paths.UpgradeStateFile, "pending upgrade must be removed", 0o600)
	currentDatabase, err := openExistingBackupTestDatabase(paths.DatabaseFile)
	if err != nil {
		t.Fatalf("open current restore database: %v", err)
	}
	if _, err := currentDatabase.Exec(`UPDATE backup_test SET value = ?`, "new database value"); err != nil {
		_ = currentDatabase.Close()
		t.Fatalf("update current restore database: %v", err)
	}
	if err := currentDatabase.Close(); err != nil {
		t.Fatalf("close current restore database: %v", err)
	}

	result, err := RestoreBackup(context.Background(), paths, backup.Manifest.ID)
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if result.BackupID != backup.Manifest.ID || result.Restored != len(backup.Manifest.Files) {
		t.Fatalf("RestoreBackup result = %#v", result)
	}
	assertBackupTestFileContent(t, paths.ConfigFile, "version: old\n")
	assertBackupTestFileContent(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "old role")
	assertBackupTestSQLiteValue(t, paths.DatabaseFile, "old database value")
	for _, path := range []string{filepath.Join(paths.DataDir, "new-only.txt"), paths.UpgradeStateFile, paths.RestoreStateFile} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("restored desktop path %q still exists: %v", path, err)
		}
	}
	if _, err := VerifyBackup(backup.Directory); err != nil {
		t.Fatalf("restore changed recovery point: %v", err)
	}
	assertNoRestoreWorkspaces(t, paths)
}

func TestRecoverInterruptedRestoreRollsBackPartialSwitch(t *testing.T) {
	paths, backup := prepareRestoreRollbackFixture(t)
	state, _, err := prepareRestoreTransaction(context.Background(), paths, backup.Manifest.ID, time.Now())
	if err != nil {
		t.Fatalf("prepareRestoreTransaction: %v", err)
	}
	dataWorkspace, _ := restoreWorkspacePaths(paths, state.TransactionID)
	liveResources := paths.ResourcesDir
	rollbackResources := filepath.Join(dataWorkspace, "rollback", "resources")
	stageResources := filepath.Join(dataWorkspace, "stage", "resources")
	if err := os.MkdirAll(filepath.Dir(rollbackResources), 0o700); err != nil {
		t.Fatalf("prepare partial restore rollback: %v", err)
	}
	if err := os.Rename(liveResources, rollbackResources); err != nil {
		t.Fatalf("move live resources during partial restore: %v", err)
	}
	if err := os.Rename(stageResources, liveResources); err != nil {
		t.Fatalf("publish staged resources during partial restore: %v", err)
	}
	assertBackupTestFileContent(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "old role")

	recovered, err := RecoverInterruptedRestore(paths)
	if err != nil || !recovered {
		t.Fatalf("RecoverInterruptedRestore = %t, %v", recovered, err)
	}
	assertBackupTestFileContent(t, paths.ConfigFile, "version: new\n")
	assertBackupTestFileContent(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "new role")
	assertBackupTestFileContent(t, filepath.Join(paths.DataDir, "new-only.txt"), "keep me")
	if _, exists, err := LoadRestoreState(paths.RestoreStateFile); err != nil || exists {
		t.Fatalf("recovered restore state exists=%t err=%v", exists, err)
	}
	if recovered, err := RecoverInterruptedRestore(paths); err != nil || recovered {
		t.Fatalf("second RecoverInterruptedRestore = %t, %v", recovered, err)
	}
	if _, err := VerifyBackup(backup.Directory); err != nil {
		t.Fatalf("interrupted restore changed recovery point: %v", err)
	}
	assertNoRestoreWorkspaces(t, paths)
}

func TestRestoreVerificationFailureRollsBackLiveData(t *testing.T) {
	paths, backup := prepareRestoreRollbackFixture(t)
	state, manifest, err := prepareRestoreTransaction(context.Background(), paths, backup.Manifest.ID, time.Now())
	if err != nil {
		t.Fatalf("prepareRestoreTransaction: %v", err)
	}
	dataWorkspace, _ := restoreWorkspacePaths(paths, state.TransactionID)
	stagedRole := filepath.Join(dataWorkspace, "stage", "resources", "roles", "restore.md")
	if err := os.WriteFile(stagedRole, []byte("tampered staged role"), 0o600); err != nil {
		t.Fatalf("tamper staged restore: %v", err)
	}
	if err := commitRestoreTransaction(paths, state, manifest); err == nil || (!strings.Contains(err.Error(), "metadata mismatch") && !strings.Contains(err.Error(), "checksum mismatch")) {
		t.Fatalf("commitRestoreTransaction error = %v, want staged payload integrity failure", err)
	}
	if err := rollbackRestoreTransaction(paths, state); err != nil {
		t.Fatalf("rollbackRestoreTransaction: %v", err)
	}
	assertBackupTestFileContent(t, paths.ConfigFile, "version: new\n")
	assertBackupTestFileContent(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "new role")
	assertBackupTestFileContent(t, filepath.Join(paths.DataDir, "new-only.txt"), "keep me")
	if _, err := VerifyBackup(backup.Directory); err != nil {
		t.Fatalf("failed restore changed recovery point: %v", err)
	}
	assertNoRestoreWorkspaces(t, paths)
}

func TestListBackupsReportsValidAndCorruptedEntries(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: one\n", 0o600)
	first, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create first catalog backup: %v", err)
	}
	writeBackupTestFile(t, paths.ConfigFile, "version: two\n", 0o600)
	second, err := CreateUpgradeBackup(context.Background(), paths, "1.1.0", "1.2.0", time.Date(2026, 7, 31, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create second catalog backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Directory, "payload", "config", "config.yaml"), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt catalog backup: %v", err)
	}
	summaries, err := ListBackups(paths)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(summaries) != 2 || !summaries[0].Valid || summaries[0].ID != second.Manifest.ID || summaries[1].Valid || summaries[1].ID != first.Manifest.ID || summaries[1].Error == "" {
		t.Fatalf("backup catalog = %#v", summaries)
	}
}

func TestListBackupsRetainsNewestTwoAndProtectsPendingUpgrade(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: one\n", 0o600)
	for index, versions := range [][2]string{{"1.0.0", "1.1.0"}, {"1.1.0", "1.2.0"}, {"1.2.0", "1.3.0"}} {
		if _, err := CreateUpgradeBackup(context.Background(), paths, versions[0], versions[1], time.Date(2026, 7, 31, 21+index, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("create retention backup %d: %v", index, err)
		}
	}
	pending, err := PrepareUpgrade(context.Background(), paths, "1.3.0", "1.4.0", time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	summaries, err := ListBackups(paths)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(summaries) != 4 {
		t.Fatalf("backup retention catalog = %#v", summaries)
	}
	if summaries[3].ID != pending.State().BackupID || !summaries[3].Protected || !summaries[3].Retained || summaries[3].Deletable {
		t.Fatalf("pending backup retention = %#v", summaries[3])
	}
	for _, summary := range summaries[:2] {
		if !summary.Retained || summary.Deletable {
			t.Fatalf("new backup retention = %#v", summary)
		}
	}
	if summaries[2].Retained || !summaries[2].Deletable {
		t.Fatalf("old unprotected backup retention = %#v", summaries[2])
	}
	if err := DeleteBackup(paths, summaries[0].ID); err == nil || !strings.Contains(err.Error(), "retention policy") {
		t.Fatalf("DeleteBackup newest error = %v", err)
	}
	if err := DeleteBackup(paths, summaries[3].ID); err == nil || !strings.Contains(err.Error(), "retention policy") {
		t.Fatalf("DeleteBackup pending error = %v", err)
	}
	deletedID := summaries[2].ID
	if err := DeleteBackup(paths, deletedID); err != nil {
		t.Fatalf("DeleteBackup old recovery point: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.BackupsDir, deletedID)); !os.IsNotExist(err) {
		t.Fatalf("deleted recovery point remains: %v", err)
	}
}

func prepareRestoreRollbackFixture(t *testing.T) (desktopruntime.Paths, BackupResult) {
	t.Helper()
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "old role", 0o600)
	backup, err := CreateUpgradeBackup(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("CreateUpgradeBackup: %v", err)
	}
	writeBackupTestFile(t, paths.ConfigFile, "version: new\n", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.ResourcesDir, "roles", "restore.md"), "new role", 0o600)
	writeBackupTestFile(t, filepath.Join(paths.DataDir, "new-only.txt"), "keep me", 0o600)
	return paths, backup
}

func assertBackupTestFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil || string(content) != want {
		t.Fatalf("file %q content = %q, want %q, err=%v", path, content, want, err)
	}
}

func assertNoRestoreWorkspaces(t *testing.T, paths desktopruntime.Paths) {
	t.Helper()
	for _, root := range []string{paths.BackupsDir, paths.ConfigDir} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read restore workspace root %q: %v", root, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), restoreWorkspacePrefix) {
				t.Fatalf("restore workspace remains at %q", filepath.Join(root, entry.Name()))
			}
		}
	}
}

func openExistingBackupTestDatabase(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", path+"?_busy_timeout=5000")
}
