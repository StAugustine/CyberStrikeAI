package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"cyberstrike-ai/internal/desktopmigration"
	"cyberstrike-ai/internal/desktopruntime"
)

const (
	maintenanceListBackups   = "list-backups"
	maintenancePrepareImport = "prepare-import"
	maintenanceCommitImport  = "commit-import"
	maintenanceCancelImport  = "cancel-import"
	maintenanceRestoreBackup = "restore-backup"
	maintenanceDeleteBackup  = "delete-backup"
)

type desktopMaintenanceResponse struct {
	Operation     string                               `json:"operation"`
	Backups       []desktopmigration.BackupSummary     `json:"backups,omitempty"`
	PendingImport *desktopmigration.ImportState        `json:"pending_import,omitempty"`
	ImportReport  *desktopmigration.ImportReport       `json:"import_report,omitempty"`
	ImportCommit  *desktopmigration.ImportCommitResult `json:"import_commit,omitempty"`
	Restore       *desktopMaintenanceRestoreResult     `json:"restore,omitempty"`
	Cancelled     bool                                 `json:"cancelled,omitempty"`
	DeletedBackup string                               `json:"deleted_backup,omitempty"`
}

type desktopMaintenanceRestoreResult struct {
	BackupID         string `json:"backup_id"`
	RollbackBackupID string `json:"rollback_backup_id"`
	FromVersion      string `json:"from_version"`
	ToVersion        string `json:"to_version"`
	RestoredFiles    int    `json:"restored_files"`
}

func runDesktopMaintenance(
	ctx context.Context,
	stdout io.Writer,
	options runOptions,
	mode string,
	sourceDirectory string,
	backupID string,
	startedAt time.Time,
) error {
	if ctx == nil {
		return errors.New("desktop maintenance context is required")
	}
	if stdout == nil {
		return errors.New("desktop maintenance output is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mode = strings.TrimSpace(mode)
	options.AppVersion = strings.TrimSpace(options.AppVersion)
	if options.AppVersion == "" {
		return errors.New("desktop app version is required")
	}
	if startedAt.IsZero() {
		return errors.New("desktop maintenance start time is required")
	}
	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		return err
	}
	if _, err := desktopmigration.RecoverInterruptedRestore(paths); err != nil {
		return fmt.Errorf("recover interrupted desktop restore: %w", err)
	}
	if err := paths.Prepare(); err != nil {
		return err
	}

	response := desktopMaintenanceResponse{Operation: mode}
	switch mode {
	case maintenanceListBackups:
		if strings.TrimSpace(sourceDirectory) != "" || strings.TrimSpace(backupID) != "" {
			return errors.New("list-backups does not accept source-dir or backup-id")
		}
		response.Backups, err = desktopmigration.ListBackups(paths)
		if err != nil {
			return err
		}
		pending, exists, err := desktopmigration.LoadImportState(paths.ImportStateFile)
		if err != nil {
			return err
		}
		if exists {
			response.PendingImport = &pending
		}
	case maintenancePrepareImport:
		if strings.TrimSpace(backupID) != "" {
			return errors.New("prepare-import does not accept backup-id")
		}
		session, err := desktopmigration.PrepareLegacyImport(ctx, paths, sourceDirectory, options.AppVersion, startedAt)
		if err != nil {
			return err
		}
		report := session.Report()
		response.ImportReport = &report
	case maintenanceCommitImport:
		if strings.TrimSpace(sourceDirectory) != "" || strings.TrimSpace(backupID) != "" {
			return errors.New("commit-import does not accept source-dir or backup-id")
		}
		session, exists, err := desktopmigration.LoadPendingImportSession(paths)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("desktop import is not pending")
		}
		result, err := session.CommitOffline(ctx, startedAt)
		if err != nil {
			return err
		}
		response.ImportCommit = &result
	case maintenanceCancelImport:
		if strings.TrimSpace(sourceDirectory) != "" || strings.TrimSpace(backupID) != "" {
			return errors.New("cancel-import does not accept source-dir or backup-id")
		}
		response.Cancelled, err = desktopmigration.CancelPendingImport(paths)
		if err != nil {
			return err
		}
	case maintenanceRestoreBackup:
		if strings.TrimSpace(sourceDirectory) != "" {
			return errors.New("restore-backup does not accept source-dir")
		}
		response.Restore, err = restoreDesktopBackupWithRecoveryPoint(ctx, paths, options.AppVersion, backupID, startedAt)
		if err != nil {
			return err
		}
	case maintenanceDeleteBackup:
		if strings.TrimSpace(sourceDirectory) != "" {
			return errors.New("delete-backup does not accept source-dir")
		}
		if err := desktopmigration.DeleteBackup(paths, backupID); err != nil {
			return err
		}
		response.DeletedBackup = strings.TrimSpace(backupID)
	default:
		return fmt.Errorf("unsupported desktop maintenance operation: %q", mode)
	}
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		return fmt.Errorf("encode desktop maintenance result: %w", err)
	}
	return nil
}

func restoreDesktopBackupWithRecoveryPoint(
	ctx context.Context,
	paths desktopruntime.Paths,
	appVersion string,
	backupID string,
	startedAt time.Time,
) (*desktopMaintenanceRestoreResult, error) {
	backupID = strings.TrimSpace(backupID)
	if backupID == "" || filepath.Base(backupID) != backupID || strings.ContainsAny(backupID, `/\\`) {
		return nil, errors.New("desktop backup id is invalid")
	}
	if _, exists, err := desktopmigration.LoadImportState(paths.ImportStateFile); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("cancel or commit the pending desktop import before restoring a backup")
	}
	manifest, err := desktopmigration.VerifyBackup(filepath.Join(paths.BackupsDir, backupID))
	if err != nil {
		return nil, fmt.Errorf("verify desktop restore selection: %w", err)
	}
	if manifest.ID != backupID {
		return nil, errors.New("desktop restore selection does not match manifest id")
	}
	rollback, err := desktopmigration.CreateUpgradeBackup(ctx, paths, appVersion, appVersion, startedAt)
	if err != nil {
		return nil, fmt.Errorf("create pre-restore desktop recovery point: %w", err)
	}
	restored, err := desktopmigration.RestoreBackup(ctx, paths, backupID)
	if err != nil {
		return nil, err
	}
	return &desktopMaintenanceRestoreResult{
		BackupID:         restored.BackupID,
		RollbackBackupID: rollback.Manifest.ID,
		FromVersion:      restored.FromVersion,
		ToVersion:        restored.ToVersion,
		RestoredFiles:    restored.Restored,
	}, nil
}
