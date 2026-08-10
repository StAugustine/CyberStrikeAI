package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopmigration"
	"cyberstrike-ai/internal/desktopruntime"
)

func TestDesktopMaintenanceImportListCommitAndRestore(t *testing.T) {
	root := t.TempDir()
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "desktop-data"),
			ConfigDir: filepath.Join(root, "desktop-config"),
			CacheDir:  filepath.Join(root, "desktop-cache"),
			LogDir:    filepath.Join(root, "desktop-logs"),
			TempDir:   filepath.Join(root, "desktop-temp"),
		},
		AppVersion: "0.1.0",
	}
	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := paths.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	writeMaintenanceTestFile(t, paths.ConfigFile, "version: current\n")
	writeMaintenanceTestDatabase(t, paths.DatabaseFile, "current desktop")

	sourceRoot := filepath.Join(root, "legacy-source")
	writeMaintenanceTestFile(t, filepath.Join(sourceRoot, "config.yaml"), "version: v1.7.9\nserver:\n  host: 0.0.0.0\n  port: 8080\nlog:\n  level: info\n  output: stdout\nmcp:\n  enabled: false\nfofa:\n  api_key: maintenance-import-secret\nsecurity:\n  tools_dir: tools\ndatabase:\n  path: data/conversations.db\nknowledge:\n  base_path: knowledge_base\nroles_dir: roles\nskills_dir: skills\nagents_dir: agents\n")
	writeMaintenanceTestDatabase(t, filepath.Join(sourceRoot, "data", "conversations.db"), "imported desktop")

	prepareOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), prepareOutput, options, maintenancePrepareImport, sourceRoot, "", time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("prepare import maintenance: %v", err)
	}
	if strings.Contains(prepareOutput.String(), sourceRoot) || strings.Contains(prepareOutput.String(), "maintenance-import-secret") {
		t.Fatalf("prepare import maintenance exposed source details: %s", prepareOutput.String())
	}
	var prepared desktopMaintenanceResponse
	if err := json.Unmarshal(prepareOutput.Bytes(), &prepared); err != nil {
		t.Fatalf("decode prepare import maintenance: %v", err)
	}
	if prepared.Operation != maintenancePrepareImport || prepared.ImportReport == nil || prepared.ImportReport.BackupID == "" {
		t.Fatalf("prepare import maintenance response = %#v", prepared)
	}

	listOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), listOutput, options, maintenanceListBackups, "", "", time.Now()); err != nil {
		t.Fatalf("list backups maintenance: %v", err)
	}
	var listed desktopMaintenanceResponse
	if err := json.Unmarshal(listOutput.Bytes(), &listed); err != nil {
		t.Fatalf("decode list maintenance: %v", err)
	}
	if len(listed.Backups) != 1 || listed.PendingImport == nil || listed.PendingImport.BackupID != prepared.ImportReport.BackupID || !listed.Backups[0].Protected {
		t.Fatalf("list maintenance response = %#v", listed)
	}

	commitOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), commitOutput, options, maintenanceCommitImport, "", "", time.Date(2026, 7, 31, 22, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("commit import maintenance: %v", err)
	}
	var committed desktopMaintenanceResponse
	if err := json.Unmarshal(commitOutput.Bytes(), &committed); err != nil {
		t.Fatalf("decode commit maintenance: %v", err)
	}
	if committed.ImportCommit == nil || committed.ImportCommit.RollbackBackupID == "" {
		t.Fatalf("commit import maintenance response = %#v", committed)
	}
	assertMaintenanceTestDatabase(t, paths.DatabaseFile, "imported desktop")
	if _, exists, err := desktopmigration.LoadImportState(paths.ImportStateFile); err != nil || exists {
		t.Fatalf("committed maintenance import state exists=%t err=%v", exists, err)
	}

	restoreOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), restoreOutput, options, maintenanceRestoreBackup, "", committed.ImportCommit.RollbackBackupID, time.Date(2026, 7, 31, 22, 2, 0, 0, time.UTC)); err != nil {
		t.Fatalf("restore backup maintenance: %v", err)
	}
	var restored desktopMaintenanceResponse
	if err := json.Unmarshal(restoreOutput.Bytes(), &restored); err != nil {
		t.Fatalf("decode restore maintenance: %v", err)
	}
	if restored.Restore == nil || restored.Restore.BackupID != committed.ImportCommit.RollbackBackupID || restored.Restore.RollbackBackupID == "" {
		t.Fatalf("restore maintenance response = %#v", restored)
	}
	assertMaintenanceTestDatabase(t, paths.DatabaseFile, "current desktop")

	deleteListOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), deleteListOutput, options, maintenanceListBackups, "", "", time.Now()); err != nil {
		t.Fatalf("list backups before delete maintenance: %v", err)
	}
	var deleteList desktopMaintenanceResponse
	if err := json.Unmarshal(deleteListOutput.Bytes(), &deleteList); err != nil {
		t.Fatalf("decode pre-delete maintenance list: %v", err)
	}
	deletableID := ""
	for _, backup := range deleteList.Backups {
		if backup.Deletable {
			deletableID = backup.ID
			break
		}
	}
	if deletableID == "" {
		t.Fatalf("maintenance catalog has no deletable recovery point: %#v", deleteList.Backups)
	}
	deleteOutput := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), deleteOutput, options, maintenanceDeleteBackup, "", deletableID, time.Now()); err != nil {
		t.Fatalf("delete backup maintenance: %v", err)
	}
	var deleted desktopMaintenanceResponse
	if err := json.Unmarshal(deleteOutput.Bytes(), &deleted); err != nil || deleted.DeletedBackup != deletableID {
		t.Fatalf("delete maintenance response = %#v, err=%v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(paths.BackupsDir, deletableID)); !os.IsNotExist(err) {
		t.Fatalf("deleted backup still exists: %v", err)
	}
}

func TestDesktopMaintenanceCancelPendingImport(t *testing.T) {
	root := t.TempDir()
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		AppVersion: "0.1.0",
	}
	sourceRoot := filepath.Join(root, "source")
	writeMaintenanceTestFile(t, filepath.Join(sourceRoot, "config.yaml"), "version: v1.7.9\ndatabase:\n  path: data/conversations.db\n")
	writeMaintenanceTestDatabase(t, filepath.Join(sourceRoot, "data", "conversations.db"), "cancel import")
	if err := runDesktopMaintenance(context.Background(), bytes.NewBuffer(nil), options, maintenancePrepareImport, sourceRoot, "", time.Now()); err != nil {
		t.Fatalf("prepare cancelled import: %v", err)
	}
	output := bytes.NewBuffer(nil)
	if err := runDesktopMaintenance(context.Background(), output, options, maintenanceCancelImport, "", "", time.Now().Add(time.Second)); err != nil {
		t.Fatalf("cancel import maintenance: %v", err)
	}
	var cancelled desktopMaintenanceResponse
	if err := json.Unmarshal(output.Bytes(), &cancelled); err != nil || !cancelled.Cancelled {
		t.Fatalf("cancel import response = %#v, err=%v", cancelled, err)
	}
	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := desktopmigration.LoadImportState(paths.ImportStateFile); err != nil || exists {
		t.Fatalf("cancelled maintenance import exists=%t err=%v", exists, err)
	}
}

func writeMaintenanceTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeMaintenanceTestDatabase(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE backup_test (value TEXT NOT NULL)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO backup_test(value) VALUES (?)`, value); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertMaintenanceTestDatabase(t *testing.T, path, want string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var got string
	if err := database.QueryRow(`SELECT value FROM backup_test`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("maintenance database value = %q, want %q", got, want)
	}
}
