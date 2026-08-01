package desktopmigration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareUpgradeCreatesOneBackupAndResumesUntilCompletion(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	writeBackupTestFile(t, paths.ResourceStateFile, `{"schema_version":1,"app_version":"1.0.0","files":{}}`, 0o600)
	startedAt := time.Date(2026, time.July, 31, 19, 0, 0, 0, time.UTC)

	session, err := PrepareUpgrade(context.Background(), paths, "1.0.0", "1.1.0", startedAt)
	if err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	if session == nil || session.Resumed() {
		t.Fatalf("new upgrade session = %#v", session)
	}
	state, exists, err := LoadUpgradeState(paths.UpgradeStateFile)
	if err != nil || !exists || state != session.State() {
		t.Fatalf("LoadUpgradeState = %#v, %t, %v", state, exists, err)
	}
	stateInfo, err := os.Stat(paths.UpgradeStateFile)
	if err != nil {
		t.Fatalf("inspect upgrade state permissions: %v", err)
	}
	if stateInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("upgrade state permissions = %v", stateInfo.Mode().Perm())
	}
	backupDirectory := filepath.Join(paths.BackupsDir, state.BackupID)
	manifest, err := VerifyBackup(backupDirectory)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if manifest.FromVersion != "1.0.0" || manifest.ToVersion != "1.1.0" {
		t.Fatalf("upgrade backup versions = %q -> %q", manifest.FromVersion, manifest.ToVersion)
	}
	backupConfig, err := os.ReadFile(filepath.Join(backupDirectory, "payload", "config", "config.yaml"))
	if err != nil || string(backupConfig) != "version: old\n" {
		t.Fatalf("pre-upgrade config = %q, err=%v", backupConfig, err)
	}

	writeBackupTestFile(t, paths.ConfigFile, "version: partially-migrated\n", 0o600)
	writeBackupTestFile(t, paths.ResourceStateFile, `{"schema_version":1,"app_version":"1.1.0","files":{}}`, 0o600)
	resumed, err := PrepareUpgrade(context.Background(), paths, "1.1.0", "1.1.0", startedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("resume PrepareUpgrade: %v", err)
	}
	if resumed == nil || !resumed.Resumed() || resumed.State() != state {
		t.Fatalf("resumed upgrade session = %#v", resumed)
	}
	entries, err := os.ReadDir(paths.BackupsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("upgrade backup entries = %#v, err=%v", entries, err)
	}
	if err := resumed.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, exists, err := LoadUpgradeState(paths.UpgradeStateFile); err != nil || exists {
		t.Fatalf("completed upgrade state exists=%t err=%v", exists, err)
	}
	if _, err := VerifyBackup(backupDirectory); err != nil {
		t.Fatalf("completed upgrade recovery point: %v", err)
	}
}

func TestPrepareUpgradeSameVersionDoesNothing(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	session, err := PrepareUpgrade(context.Background(), paths, "1.0.0", "1.0.0", time.Now())
	if err != nil || session != nil {
		t.Fatalf("PrepareUpgrade = %#v, %v", session, err)
	}
	assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	if _, exists, err := LoadUpgradeState(paths.UpgradeStateFile); err != nil || exists {
		t.Fatalf("same-version upgrade state exists=%t err=%v", exists, err)
	}
}

func TestPrepareUpgradeRejectsAutomaticDowngrade(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: current\n", 0o600)
	session, err := PrepareUpgrade(context.Background(), paths, "2.0.0", "1.9.9", time.Now())
	if err == nil || !strings.Contains(err.Error(), "requires restoring a matching backup") || session != nil {
		t.Fatalf("PrepareUpgrade = %#v, %v, want downgrade rejection", session, err)
	}
	assertBackupTestDirectoryEmpty(t, paths.BackupsDir)
	if _, exists, err := LoadUpgradeState(paths.UpgradeStateFile); err != nil || exists {
		t.Fatalf("downgrade state exists=%t err=%v", exists, err)
	}
}

func TestSemanticVersionPrecedenceForUpgradeBoundary(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	for index := 0; index < len(ordered)-1; index++ {
		left, leftOK := parseSemanticVersion(ordered[index])
		right, rightOK := parseSemanticVersion(ordered[index+1])
		if !leftOK || !rightOK || compareSemanticVersions(left, right) >= 0 {
			t.Fatalf("semantic version precedence %q < %q failed", ordered[index], ordered[index+1])
		}
	}
	withBuild, ok := parseSemanticVersion("v1.2.3-rc.1+desktop.5")
	withoutBuild, plainOK := parseSemanticVersion("1.2.3-rc.1")
	if !ok || !plainOK || compareSemanticVersions(withBuild, withoutBuild) != 0 {
		t.Fatal("semantic version build metadata changed precedence")
	}
}

func TestPrepareUpgradeRejectsDifferentTargetWhilePending(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	if _, err := PrepareUpgrade(context.Background(), paths, "1.0.0", "1.1.0", time.Now()); err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	_, err := PrepareUpgrade(context.Background(), paths, "1.1.0", "1.2.0", time.Now())
	if err == nil || !strings.Contains(err.Error(), "unfinished desktop upgrade targets") {
		t.Fatalf("PrepareUpgrade error = %v, want target mismatch", err)
	}
	entries, readErr := os.ReadDir(paths.BackupsDir)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("upgrade backup entries = %#v, err=%v", entries, readErr)
	}
}

func TestPrepareUpgradeRejectsCorruptedPendingBackup(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	session, err := PrepareUpgrade(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	backupConfig := filepath.Join(paths.BackupsDir, session.State().BackupID, "payload", "config", "config.yaml")
	if err := os.WriteFile(backupConfig, []byte("tampered\n"), 0o600); err != nil {
		t.Fatalf("tamper pending backup: %v", err)
	}
	_, err = PrepareUpgrade(context.Background(), paths, "1.1.0", "1.1.0", time.Now())
	if err == nil || !strings.Contains(err.Error(), "unfinished desktop upgrade backup") {
		t.Fatalf("PrepareUpgrade error = %v, want corrupted backup rejection", err)
	}
}

func TestUpgradeSessionCompleteRejectsStateReplacement(t *testing.T) {
	paths := prepareBackupTestPaths(t)
	writeBackupTestFile(t, paths.ConfigFile, "version: old\n", 0o600)
	session, err := PrepareUpgrade(context.Background(), paths, "1.0.0", "1.1.0", time.Now())
	if err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	stateData, err := os.ReadFile(paths.UpgradeStateFile)
	if err != nil {
		t.Fatalf("read upgrade state: %v", err)
	}
	stateData = []byte(strings.Replace(string(stateData), `"status": "pending"`, `"status": "complete"`, 1))
	if err := os.WriteFile(paths.UpgradeStateFile, stateData, 0o600); err != nil {
		t.Fatalf("replace upgrade state: %v", err)
	}
	if err := session.Complete(); err == nil {
		t.Fatal("Complete accepted replaced upgrade state")
	}
	if _, err := os.Stat(paths.UpgradeStateFile); err != nil {
		t.Fatalf("replaced upgrade state was removed: %v", err)
	}
}
