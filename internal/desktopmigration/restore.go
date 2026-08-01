package desktopmigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
)

const RestoreStateSchemaVersion = 1

const restoreWorkspacePrefix = ".desktop-restore-"

type RestoreState struct {
	SchemaVersion       int      `json:"schema_version"`
	BackupID            string   `json:"backup_id"`
	TransactionID       string   `json:"transaction_id"`
	StartedAt           string   `json:"started_at"`
	TargetDataEntries   []string `json:"target_data_entries"`
	OriginalDataEntries []string `json:"original_data_entries"`
	TargetConfig        bool     `json:"target_config"`
	OriginalConfig      bool     `json:"original_config"`
}

type RestoreResult struct {
	BackupID    string
	FromVersion string
	ToVersion   string
	Restored    int
}

// RestoreBackup replaces desktop-managed configuration and data with a
// verified recovery point. The backup directory itself is never moved or
// modified. A durable restore state makes every interrupted commit rollback
// to the exact pre-restore top-level layout on the next startup.
func RestoreBackup(ctx context.Context, paths desktopruntime.Paths, backupID string) (RestoreResult, error) {
	return restoreBackup(ctx, paths, backupID, platformAvailableDiskBytes)
}

func restoreBackup(ctx context.Context, paths desktopruntime.Paths, backupID string, probe diskSpaceProbe) (RestoreResult, error) {
	if ctx == nil {
		return RestoreResult{}, errors.New("desktop restore context is required")
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if _, err := RecoverInterruptedRestore(paths); err != nil {
		return RestoreResult{}, err
	}
	state, manifest, err := prepareRestoreTransactionWithDiskSpace(ctx, paths, backupID, time.Now(), probe)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := commitRestoreTransaction(paths, state, manifest); err != nil {
		rollbackErr := rollbackRestoreTransaction(paths, state)
		if rollbackErr != nil {
			return RestoreResult{}, fmt.Errorf("restore desktop backup: %v; rollback failed: %w", err, rollbackErr)
		}
		return RestoreResult{}, fmt.Errorf("restore desktop backup: %w", err)
	}
	return RestoreResult{
		BackupID:    manifest.ID,
		FromVersion: manifest.FromVersion,
		ToVersion:   manifest.ToVersion,
		Restored:    len(manifest.Files),
	}, nil
}

// RecoverInterruptedRestore rolls back a restore whose durable state still
// exists. Without a state marker, only orphaned private staging directories
// are removed; live data is not changed.
func RecoverInterruptedRestore(paths desktopruntime.Paths) (bool, error) {
	state, exists, err := LoadRestoreState(paths.RestoreStateFile)
	if err != nil {
		return false, err
	}
	if !exists {
		cleanupRestoreWorkspaces(paths, "")
		return false, nil
	}
	if err := validateRestoreStateForPaths(paths, state); err != nil {
		return false, err
	}
	if err := rollbackRestoreTransaction(paths, state); err != nil {
		return false, err
	}
	return true, nil
}

func LoadRestoreState(path string) (RestoreState, bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return RestoreState{}, false, fmt.Errorf("desktop restore state path must be absolute: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RestoreState{}, false, nil
		}
		return RestoreState{}, false, fmt.Errorf("inspect desktop restore state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return RestoreState{}, false, errors.New("desktop restore state is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return RestoreState{}, false, err
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state RestoreState
	decodeErr := decoder.Decode(&state)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			decodeErr = errors.New("desktop restore state contains trailing data")
		}
	}
	closeErr := file.Close()
	if decodeErr != nil {
		return RestoreState{}, false, fmt.Errorf("decode desktop restore state: %w", decodeErr)
	}
	if closeErr != nil {
		return RestoreState{}, false, closeErr
	}
	if err := validateRestoreState(state); err != nil {
		return RestoreState{}, false, err
	}
	return state, true, nil
}

func prepareRestoreTransaction(
	ctx context.Context,
	paths desktopruntime.Paths,
	backupID string,
	startedAt time.Time,
) (RestoreState, BackupManifest, error) {
	return prepareRestoreTransactionWithDiskSpace(ctx, paths, backupID, startedAt, platformAvailableDiskBytes)
}

func prepareRestoreTransactionWithDiskSpace(
	ctx context.Context,
	paths desktopruntime.Paths,
	backupID string,
	startedAt time.Time,
	probe diskSpaceProbe,
) (RestoreState, BackupManifest, error) {
	if err := validateRestorePaths(paths); err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	if startedAt.IsZero() {
		return RestoreState{}, BackupManifest{}, errors.New("desktop restore start time is required")
	}
	if _, exists, err := LoadRestoreState(paths.RestoreStateFile); err != nil {
		return RestoreState{}, BackupManifest{}, err
	} else if exists {
		return RestoreState{}, BackupManifest{}, errors.New("desktop restore is already pending")
	}
	backupID = strings.TrimSpace(backupID)
	if backupID == "" || filepath.Base(backupID) != backupID || strings.ContainsAny(backupID, `/\`) {
		return RestoreState{}, BackupManifest{}, errors.New("desktop restore backup id is invalid")
	}
	backupDirectory := filepath.Join(paths.BackupsDir, backupID)
	manifest, err := VerifyBackup(backupDirectory)
	if err != nil {
		return RestoreState{}, BackupManifest{}, fmt.Errorf("verify desktop restore backup: %w", err)
	}
	if manifest.ID != backupID {
		return RestoreState{}, BackupManifest{}, errors.New("desktop restore backup directory does not match manifest id")
	}
	dataBytes, configBytes := restorePayloadDiskBytes(manifest)
	if err := ensureAvailableDiskSpace(paths.BackupsDir, dataBytes, probe); err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	if err := ensureAvailableDiskSpace(paths.ConfigDir, configBytes, probe); err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	transactionID, dataWorkspace, configWorkspace, err := reserveRestoreWorkspaces(paths, startedAt)
	if err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	removeWorkspaces := true
	defer func() {
		if removeWorkspaces {
			_ = os.RemoveAll(dataWorkspace)
			_ = os.RemoveAll(configWorkspace)
		}
	}()

	targetEntries := make(map[string]struct{})
	targetConfig := false
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return RestoreState{}, BackupManifest{}, err
		}
		source := filepath.Join(backupDirectory, "payload", filepath.FromSlash(file.Path))
		var destination string
		if file.Path == "config/config.yaml" {
			targetConfig = true
			destination = filepath.Join(configWorkspace, "stage", "config.yaml")
		} else {
			relative := strings.TrimPrefix(file.Path, "data/")
			entry := strings.SplitN(relative, "/", 2)[0]
			if !validRestoreDataEntry(entry, paths) {
				return RestoreState{}, BackupManifest{}, fmt.Errorf("desktop restore contains reserved data entry %q", entry)
			}
			targetEntries[entry] = struct{}{}
			destination = filepath.Join(dataWorkspace, "stage", filepath.FromSlash(relative))
		}
		if err := copyRestoreFile(ctx, source, destination, file); err != nil {
			return RestoreState{}, BackupManifest{}, fmt.Errorf("stage desktop restore file %q: %w", file.Path, err)
		}
	}
	originalEntries, err := liveRestoreDataEntries(paths)
	if err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	originalConfig, err := pathExists(paths.ConfigFile)
	if err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	state := RestoreState{
		SchemaVersion:       RestoreStateSchemaVersion,
		BackupID:            backupID,
		TransactionID:       transactionID,
		StartedAt:           startedAt.UTC().Format(time.RFC3339Nano),
		TargetDataEntries:   sortedRestoreEntrySet(targetEntries),
		OriginalDataEntries: originalEntries,
		TargetConfig:        targetConfig,
		OriginalConfig:      originalConfig,
	}
	stateData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return RestoreState{}, BackupManifest{}, err
	}
	stateData = append(stateData, '\n')
	if err := writePrivateStateAtomically(paths.RestoreStateFile, stateData); err != nil {
		return RestoreState{}, BackupManifest{}, fmt.Errorf("write desktop restore state: %w", err)
	}
	removeWorkspaces = false
	return state, manifest, nil
}

func commitRestoreTransaction(paths desktopruntime.Paths, state RestoreState, manifest BackupManifest) error {
	if err := validateRestoreStateForPaths(paths, state); err != nil {
		return err
	}
	current, exists, err := LoadRestoreState(paths.RestoreStateFile)
	if err != nil {
		return err
	}
	if !exists || !restoreStatesEqual(current, state) {
		return errors.New("desktop restore state changed before commit")
	}
	dataWorkspace, configWorkspace := restoreWorkspacePaths(paths, state.TransactionID)
	for _, entry := range restoreEntryUnion(state.TargetDataEntries, state.OriginalDataEntries) {
		livePath := filepath.Join(paths.DataDir, entry)
		rollbackPath := filepath.Join(dataWorkspace, "rollback", entry)
		stagePath := filepath.Join(dataWorkspace, "stage", entry)
		if exists, err := pathExists(livePath); err != nil {
			return err
		} else if exists {
			if err := os.MkdirAll(filepath.Dir(rollbackPath), backupDirectoryMode); err != nil {
				return err
			}
			if err := os.Rename(livePath, rollbackPath); err != nil {
				return err
			}
		}
		if exists, err := pathExists(stagePath); err != nil {
			return err
		} else if exists {
			if err := os.Rename(stagePath, livePath); err != nil {
				return err
			}
		}
	}
	configRollback := filepath.Join(configWorkspace, "rollback", "config.yaml")
	configStage := filepath.Join(configWorkspace, "stage", "config.yaml")
	if exists, err := pathExists(paths.ConfigFile); err != nil {
		return err
	} else if exists {
		if err := os.MkdirAll(filepath.Dir(configRollback), backupDirectoryMode); err != nil {
			return err
		}
		if err := os.Rename(paths.ConfigFile, configRollback); err != nil {
			return err
		}
	}
	if exists, err := pathExists(configStage); err != nil {
		return err
	} else if exists {
		if err := os.Rename(configStage, paths.ConfigFile); err != nil {
			return err
		}
	}
	if err := verifyRestoredLiveData(paths, manifest, state); err != nil {
		return err
	}
	if err := os.Remove(paths.RestoreStateFile); err != nil {
		return err
	}
	cleanupRestoreWorkspaces(paths, state.TransactionID)
	return nil
}

func rollbackRestoreTransaction(paths desktopruntime.Paths, state RestoreState) error {
	if err := validateRestoreStateForPaths(paths, state); err != nil {
		return err
	}
	dataWorkspace, configWorkspace := restoreWorkspacePaths(paths, state.TransactionID)
	original := restoreEntrySet(state.OriginalDataEntries)
	for _, entry := range restoreEntryUnion(state.TargetDataEntries, state.OriginalDataEntries) {
		livePath := filepath.Join(paths.DataDir, entry)
		rollbackPath := filepath.Join(dataWorkspace, "rollback", entry)
		if _, existed := original[entry]; existed {
			if rollbackExists, err := pathExists(rollbackPath); err != nil {
				return err
			} else if rollbackExists {
				if err := removeRestorePath(livePath); err != nil {
					return err
				}
				if err := os.Rename(rollbackPath, livePath); err != nil {
					return err
				}
			}
		} else if err := removeRestorePath(livePath); err != nil {
			return err
		}
	}
	configRollback := filepath.Join(configWorkspace, "rollback", "config.yaml")
	if state.OriginalConfig {
		if rollbackExists, err := pathExists(configRollback); err != nil {
			return err
		} else if rollbackExists {
			if err := removeRestorePath(paths.ConfigFile); err != nil {
				return err
			}
			if err := os.Rename(configRollback, paths.ConfigFile); err != nil {
				return err
			}
		}
	} else if err := removeRestorePath(paths.ConfigFile); err != nil {
		return err
	}
	if err := os.Remove(paths.RestoreStateFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	cleanupRestoreWorkspaces(paths, state.TransactionID)
	return nil
}

func verifyRestoredLiveData(paths desktopruntime.Paths, manifest BackupManifest, state RestoreState) error {
	liveEntries, err := liveRestoreDataEntries(paths)
	if err != nil {
		return err
	}
	if strings.Join(liveEntries, "\x00") != strings.Join(state.TargetDataEntries, "\x00") {
		return fmt.Errorf("restored desktop data entries do not match backup: got %v, want %v", liveEntries, state.TargetDataEntries)
	}
	configExists, err := pathExists(paths.ConfigFile)
	if err != nil {
		return err
	}
	if configExists != state.TargetConfig {
		return errors.New("restored desktop config presence does not match backup")
	}
	for _, file := range manifest.Files {
		var path string
		if file.Path == "config/config.yaml" {
			path = paths.ConfigFile
		} else {
			path = filepath.Join(paths.DataDir, filepath.FromSlash(strings.TrimPrefix(file.Path, "data/")))
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != file.Size {
			return fmt.Errorf("restored desktop file %q metadata mismatch", file.Path)
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		if hash != file.SHA256 {
			return fmt.Errorf("restored desktop file %q checksum mismatch", file.Path)
		}
	}
	return nil
}

func validateRestorePaths(paths desktopruntime.Paths) error {
	if !filepath.IsAbs(paths.RestoreStateFile) || !pathWithin(paths.DataDir, paths.RestoreStateFile) {
		return errors.New("desktop restore state file must be inside the data directory")
	}
	if !filepath.IsAbs(paths.ConfigFile) || !filepath.IsAbs(paths.ConfigDir) {
		return errors.New("desktop restore config paths must be absolute")
	}
	return validateBackupPaths(paths)
}

func validateRestoreStateForPaths(paths desktopruntime.Paths, state RestoreState) error {
	if err := validateRestorePaths(paths); err != nil {
		return err
	}
	if err := validateRestoreState(state); err != nil {
		return err
	}
	for _, entries := range [][]string{state.TargetDataEntries, state.OriginalDataEntries} {
		for _, entry := range entries {
			if !validRestoreDataEntry(entry, paths) {
				return fmt.Errorf("desktop restore state contains reserved data entry %q", entry)
			}
		}
	}
	return nil
}

func validateRestoreState(state RestoreState) error {
	if state.SchemaVersion != RestoreStateSchemaVersion {
		return fmt.Errorf("unsupported desktop restore state schema version: %d", state.SchemaVersion)
	}
	if state.BackupID == "" || filepath.Base(state.BackupID) != state.BackupID || strings.ContainsAny(state.BackupID, `/\`) {
		return errors.New("desktop restore state backup id is invalid")
	}
	if !strings.HasPrefix(state.TransactionID, restoreWorkspacePrefix) || filepath.Base(state.TransactionID) != state.TransactionID || strings.ContainsAny(state.TransactionID, `/\`) {
		return errors.New("desktop restore transaction id is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.StartedAt); err != nil {
		return errors.New("desktop restore state start time is invalid")
	}
	for _, entries := range [][]string{state.TargetDataEntries, state.OriginalDataEntries} {
		previous := ""
		for _, entry := range entries {
			if entry == "" || filepath.Base(entry) != entry || strings.ContainsAny(entry, `/\`) || entry == previous {
				return errors.New("desktop restore state contains invalid data entry")
			}
			if previous != "" && entry < previous {
				return errors.New("desktop restore state data entries are not sorted")
			}
			previous = entry
		}
	}
	return nil
}

func reserveRestoreWorkspaces(paths desktopruntime.Paths, startedAt time.Time) (string, string, string, error) {
	baseID := restoreWorkspacePrefix + startedAt.UTC().Format("20060102T150405.000000000Z")
	for sequence := 0; sequence < 100; sequence++ {
		transactionID := baseID
		if sequence != 0 {
			transactionID = fmt.Sprintf("%s-%02d", baseID, sequence)
		}
		dataWorkspace, configWorkspace := restoreWorkspacePaths(paths, transactionID)
		if err := os.Mkdir(dataWorkspace, backupDirectoryMode); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", "", "", err
		}
		if err := os.Mkdir(configWorkspace, backupDirectoryMode); err != nil {
			_ = os.RemoveAll(dataWorkspace)
			if os.IsExist(err) {
				continue
			}
			return "", "", "", err
		}
		return transactionID, dataWorkspace, configWorkspace, nil
	}
	return "", "", "", errors.New("unable to reserve desktop restore workspaces")
}

func restoreWorkspacePaths(paths desktopruntime.Paths, transactionID string) (string, string) {
	return filepath.Join(paths.BackupsDir, transactionID), filepath.Join(paths.ConfigDir, transactionID)
}

func cleanupRestoreWorkspaces(paths desktopruntime.Paths, transactionID string) {
	if transactionID != "" {
		dataWorkspace, configWorkspace := restoreWorkspacePaths(paths, transactionID)
		_ = os.RemoveAll(dataWorkspace)
		_ = os.RemoveAll(configWorkspace)
		return
	}
	for _, root := range []string{paths.BackupsDir, paths.ConfigDir} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), restoreWorkspacePrefix) {
				_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
		}
	}
}

func copyRestoreFile(ctx context.Context, source, destination string, manifest BackupFile) error {
	if err := os.MkdirAll(filepath.Dir(destination), backupDirectoryMode); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupFileMode)
	if err != nil {
		_ = input.Close()
		return err
	}
	hasher := sha256.New()
	written, copyErr := copyWithContext(ctx, io.MultiWriter(output, hasher), input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	inputCloseErr := input.Close()
	outputCloseErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if inputCloseErr != nil {
		return inputCloseErr
	}
	if outputCloseErr != nil {
		return outputCloseErr
	}
	if written != manifest.Size || hex.EncodeToString(hasher.Sum(nil)) != manifest.SHA256 {
		return errors.New("desktop restore staged file does not match backup manifest")
	}
	mode := os.FileMode(manifest.Mode) & 0o700
	if mode == 0 {
		mode = backupFileMode
	}
	return os.Chmod(destination, mode)
}

func liveRestoreDataEntries(paths desktopruntime.Paths) ([]string, error) {
	entries, err := os.ReadDir(paths.DataDir)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	restoreStateName := filepath.Base(paths.RestoreStateFile)
	for _, entry := range entries {
		if entry.Name() == filepath.Base(paths.BackupsDir) || entry.Name() == restoreStateName {
			continue
		}
		if !validRestoreDataEntry(entry.Name(), paths) {
			return nil, fmt.Errorf("desktop data contains reserved entry %q", entry.Name())
		}
		result = append(result, entry.Name())
	}
	sort.Strings(result)
	return result, nil
}

func validRestoreDataEntry(entry string, paths desktopruntime.Paths) bool {
	return entry != "" && filepath.Base(entry) == entry && !strings.ContainsAny(entry, `/\`) &&
		entry != "." && entry != ".." && entry != filepath.Base(paths.BackupsDir) && entry != filepath.Base(paths.RestoreStateFile)
}

func restoreEntryUnion(left, right []string) []string {
	set := restoreEntrySet(left)
	for _, entry := range right {
		set[entry] = struct{}{}
	}
	return sortedRestoreEntrySet(set)
}

func restoreStatesEqual(left, right RestoreState) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.BackupID == right.BackupID &&
		left.TransactionID == right.TransactionID &&
		left.StartedAt == right.StartedAt &&
		left.TargetConfig == right.TargetConfig &&
		left.OriginalConfig == right.OriginalConfig &&
		slices.Equal(left.TargetDataEntries, right.TargetDataEntries) &&
		slices.Equal(left.OriginalDataEntries, right.OriginalDataEntries)
}

func restoreEntrySet(entries []string) map[string]struct{} {
	set := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		set[entry] = struct{}{}
	}
	return set
}

func sortedRestoreEntrySet(set map[string]struct{}) []string {
	entries := make([]string, 0, len(set))
	for entry := range set {
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	return entries
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func removeRestorePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}
