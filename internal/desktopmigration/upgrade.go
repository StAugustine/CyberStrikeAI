package desktopmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
)

const UpgradeStateSchemaVersion = 1

type UpgradeState struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	FromVersion   string `json:"from_version"`
	ToVersion     string `json:"to_version"`
	BackupID      string `json:"backup_id"`
	StartedAt     string `json:"started_at"`
}

type UpgradeSession struct {
	paths   desktopruntime.Paths
	state   UpgradeState
	resumed bool
}

func (s *UpgradeSession) State() UpgradeState {
	if s == nil {
		return UpgradeState{}
	}
	return s.state
}

func (s *UpgradeSession) Resumed() bool {
	return s != nil && s.resumed
}

// PrepareUpgrade creates a recovery point and durable pending marker before
// any version-specific resource, configuration, or database migration runs.
// An existing compatible marker resumes the same idempotent upgrade without
// creating another backup.
func PrepareUpgrade(
	ctx context.Context,
	paths desktopruntime.Paths,
	installedVersion, targetVersion string,
	startedAt time.Time,
) (*UpgradeSession, error) {
	if ctx == nil {
		return nil, errors.New("desktop upgrade context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	installedVersion = strings.TrimSpace(installedVersion)
	targetVersion = strings.TrimSpace(targetVersion)
	if installedVersion == "" || targetVersion == "" {
		return nil, errors.New("desktop upgrade versions are required")
	}
	if startedAt.IsZero() {
		return nil, errors.New("desktop upgrade start time is required")
	}
	if err := validateUpgradeStatePath(paths); err != nil {
		return nil, err
	}
	state, exists, err := LoadUpgradeState(paths.UpgradeStateFile)
	if err != nil {
		return nil, err
	}
	if exists {
		if state.ToVersion != targetVersion {
			return nil, fmt.Errorf("unfinished desktop upgrade targets version %q, not %q", state.ToVersion, targetVersion)
		}
		if installedVersion != state.FromVersion && installedVersion != state.ToVersion {
			return nil, fmt.Errorf("desktop resource version %q is incompatible with unfinished upgrade %q to %q", installedVersion, state.FromVersion, state.ToVersion)
		}
		if err := verifyUpgradeStateBackup(paths, state); err != nil {
			return nil, err
		}
		return &UpgradeSession{paths: paths, state: state, resumed: true}, nil
	}
	if installedVersion == targetVersion {
		return nil, nil
	}
	if installed, ok := parseSemanticVersion(installedVersion); ok {
		if target, targetOK := parseSemanticVersion(targetVersion); targetOK && compareSemanticVersions(target, installed) < 0 {
			return nil, fmt.Errorf("desktop downgrade from %q to %q requires restoring a matching backup", installedVersion, targetVersion)
		}
	}
	backup, err := CreateUpgradeBackup(ctx, paths, installedVersion, targetVersion, startedAt)
	if err != nil {
		return nil, fmt.Errorf("create desktop pre-upgrade backup: %w", err)
	}
	state = UpgradeState{
		SchemaVersion: UpgradeStateSchemaVersion,
		Status:        "pending",
		FromVersion:   installedVersion,
		ToVersion:     targetVersion,
		BackupID:      backup.Manifest.ID,
		StartedAt:     startedAt.UTC().Format(time.RFC3339Nano),
	}
	stateData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode desktop upgrade state: %w", err)
	}
	stateData = append(stateData, '\n')
	if err := writePrivateStateAtomically(paths.UpgradeStateFile, stateData); err != nil {
		return nil, fmt.Errorf("write desktop upgrade state: %w", err)
	}
	return &UpgradeSession{paths: paths, state: state}, nil
}

func LoadUpgradeState(path string) (UpgradeState, bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return UpgradeState{}, false, fmt.Errorf("desktop upgrade state path must be absolute: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UpgradeState{}, false, nil
		}
		return UpgradeState{}, false, fmt.Errorf("inspect desktop upgrade state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return UpgradeState{}, false, errors.New("desktop upgrade state is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return UpgradeState{}, false, err
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state UpgradeState
	decodeErr := decoder.Decode(&state)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			decodeErr = errors.New("desktop upgrade state contains trailing data")
		}
	}
	closeErr := file.Close()
	if decodeErr != nil {
		return UpgradeState{}, false, fmt.Errorf("decode desktop upgrade state: %w", decodeErr)
	}
	if closeErr != nil {
		return UpgradeState{}, false, closeErr
	}
	if err := validateUpgradeState(state); err != nil {
		return UpgradeState{}, false, err
	}
	return state, true, nil
}

func (s *UpgradeSession) Complete() error {
	if s == nil {
		return nil
	}
	current, exists, err := LoadUpgradeState(s.paths.UpgradeStateFile)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("desktop upgrade state disappeared before completion")
	}
	if current != s.state {
		return errors.New("desktop upgrade state changed before completion")
	}
	if err := os.Remove(s.paths.UpgradeStateFile); err != nil {
		return fmt.Errorf("complete desktop upgrade: %w", err)
	}
	return nil
}

func validateUpgradeState(state UpgradeState) error {
	if state.SchemaVersion != UpgradeStateSchemaVersion {
		return fmt.Errorf("unsupported desktop upgrade state schema version: %d", state.SchemaVersion)
	}
	if state.Status != "pending" {
		return fmt.Errorf("unsupported desktop upgrade status: %q", state.Status)
	}
	if strings.TrimSpace(state.FromVersion) == "" || strings.TrimSpace(state.ToVersion) == "" {
		return errors.New("desktop upgrade state versions are required")
	}
	if state.BackupID == "" || filepath.Base(state.BackupID) != state.BackupID || strings.ContainsAny(state.BackupID, `/\`) {
		return errors.New("desktop upgrade state backup id is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.StartedAt); err != nil {
		return errors.New("desktop upgrade state start time is invalid")
	}
	return nil
}

func validateUpgradeStatePath(paths desktopruntime.Paths) error {
	if !filepath.IsAbs(paths.UpgradeStateFile) {
		return fmt.Errorf("desktop upgrade state path must be absolute: %q", paths.UpgradeStateFile)
	}
	if !pathWithin(paths.DataDir, paths.UpgradeStateFile) || filepath.Clean(paths.DataDir) == filepath.Clean(paths.UpgradeStateFile) {
		return errors.New("desktop upgrade state file must be inside the data directory")
	}
	return validateBackupPaths(paths)
}

func verifyUpgradeStateBackup(paths desktopruntime.Paths, state UpgradeState) error {
	backupDirectory := filepath.Join(paths.BackupsDir, state.BackupID)
	manifest, err := VerifyBackup(backupDirectory)
	if err != nil {
		return fmt.Errorf("verify unfinished desktop upgrade backup: %w", err)
	}
	if manifest.ID != state.BackupID || manifest.FromVersion != state.FromVersion || manifest.ToVersion != state.ToVersion {
		return errors.New("unfinished desktop upgrade backup does not match its state")
	}
	return nil
}

func writePrivateStateAtomically(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, backupDirectoryMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".desktop-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(backupFileMode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("desktop private state already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

type semanticVersion struct {
	core       [3]uint64
	prerelease []string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if value == "" {
		return semanticVersion{}, false
	}
	if buildIndex := strings.IndexByte(value, '+'); buildIndex >= 0 {
		if buildIndex == len(value)-1 {
			return semanticVersion{}, false
		}
		value = value[:buildIndex]
	}
	var prerelease []string
	if prereleaseIndex := strings.IndexByte(value, '-'); prereleaseIndex >= 0 {
		if prereleaseIndex == len(value)-1 {
			return semanticVersion{}, false
		}
		prerelease = strings.Split(value[prereleaseIndex+1:], ".")
		value = value[:prereleaseIndex]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	version := semanticVersion{prerelease: prerelease}
	for index, part := range parts {
		if !validNumericVersionIdentifier(part) {
			return semanticVersion{}, false
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		version.core[index] = number
	}
	for _, identifier := range prerelease {
		if identifier == "" {
			return semanticVersion{}, false
		}
		for _, character := range identifier {
			if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
				return semanticVersion{}, false
			}
		}
		if numericIdentifier(identifier) && !validNumericVersionIdentifier(identifier) {
			return semanticVersion{}, false
		}
	}
	return version, true
}

func compareSemanticVersions(left, right semanticVersion) int {
	for index := range left.core {
		if left.core[index] < right.core[index] {
			return -1
		}
		if left.core[index] > right.core[index] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	limit := min(len(left.prerelease), len(right.prerelease))
	for index := 0; index < limit; index++ {
		leftID := left.prerelease[index]
		rightID := right.prerelease[index]
		if leftID == rightID {
			continue
		}
		leftNumeric := numericIdentifier(leftID)
		rightNumeric := numericIdentifier(rightID)
		if leftNumeric && rightNumeric {
			if len(leftID) < len(rightID) || (len(leftID) == len(rightID) && leftID < rightID) {
				return -1
			}
			return 1
		}
		if leftNumeric {
			return -1
		}
		if rightNumeric {
			return 1
		}
		if leftID < rightID {
			return -1
		}
		return 1
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func validNumericVersionIdentifier(value string) bool {
	return value != "" && (value == "0" || value[0] != '0') && numericIdentifier(value)
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
