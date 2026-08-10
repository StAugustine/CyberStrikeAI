package desktopmigration

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"cyberstrike-ai/internal/desktopruntime"
)

const diskSpaceSafetyReserveBytes uint64 = 8 * 1024 * 1024

type diskSpaceProbe func(string) (uint64, error)

func ensureAvailableDiskSpace(path string, payloadBytes uint64, probe diskSpaceProbe) error {
	if probe == nil {
		return errors.New("desktop disk space probe is required")
	}
	requiredBytes := saturatingAdd(payloadBytes, diskSpaceSafetyReserveBytes)
	availableBytes, err := probe(path)
	if err != nil {
		return fmt.Errorf("inspect available desktop disk space: %w", err)
	}
	if availableBytes < requiredBytes {
		return fmt.Errorf(
			"insufficient disk space for desktop data operation: require %d bytes, available %d bytes",
			requiredBytes,
			availableBytes,
		)
	}
	return nil
}

func estimateUpgradeBackupDiskBytes(paths desktopruntime.Paths) (uint64, error) {
	databasePaths := map[string]struct{}{
		filepath.Clean(paths.DatabaseFile):          {},
		filepath.Clean(paths.KnowledgeDatabaseFile): {},
	}
	var regularBytes uint64
	if size, exists, err := regularFileSize(paths.ConfigFile); err != nil {
		return 0, err
	} else if exists {
		regularBytes = saturatingAdd(regularBytes, size)
	}
	if err := filepath.WalkDir(paths.DataDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		path = filepath.Clean(path)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop backup source contains symbolic link: %s", path)
		}
		if path == filepath.Clean(paths.BackupsDir) {
			return filepath.SkipDir
		}
		if path == filepath.Clean(paths.DataDir) || entry.IsDir() {
			return nil
		}
		if path == filepath.Clean(paths.UpgradeStateFile) || path == filepath.Clean(paths.RestoreStateFile) || path == filepath.Clean(paths.ImportStateFile) {
			return nil
		}
		if _, isDatabase := databasePaths[path]; isDatabase || isSQLiteSidecar(path, databasePaths) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("desktop backup source is not a regular file: %s", path)
		}
		regularBytes = saturatingAdd(regularBytes, uint64(info.Size()))
		return nil
	}); err != nil {
		return 0, err
	}
	var databaseBytes uint64
	for databasePath := range databasePaths {
		bytes, err := sqliteSourceDiskBytes(databasePath)
		if err != nil {
			return 0, err
		}
		databaseBytes = saturatingAdd(databaseBytes, bytes)
	}
	// SQLite backup temporarily holds a stable source snapshot and the online
	// backup destination at the same time. Two source footprints are reserved
	// so ENOSPC is detected before any recovery point is staged.
	return saturatingAdd(regularBytes, saturatingMultiply(databaseBytes, 2)), nil
}

func estimateLegacyImportDiskBytes(source legacyImportSource) (uint64, error) {
	// The rewritten configuration is bounded by the same maximum accepted for
	// the legacy source, which keeps the preflight conservative.
	requiredBytes := uint64(legacyImportMaximumConfigBytes)
	databasePaths := make(map[string]struct{}, 2)
	for _, databasePath := range []string{source.database, source.knowledgeDatabase} {
		if databasePath == "" {
			continue
		}
		databasePath = filepath.Clean(databasePath)
		if _, exists := databasePaths[databasePath]; exists {
			continue
		}
		databasePaths[databasePath] = struct{}{}
		bytes, err := sqliteSourceDiskBytes(databasePath)
		if err != nil {
			return 0, err
		}
		requiredBytes = saturatingAdd(requiredBytes, saturatingMultiply(bytes, 2))
	}
	for _, directory := range source.directories {
		bytes, err := regularTreeDiskBytes(directory.source)
		if err != nil {
			return 0, err
		}
		requiredBytes = saturatingAdd(requiredBytes, bytes)
	}
	return requiredBytes, nil
}

func restorePayloadDiskBytes(manifest BackupManifest) (dataBytes, configBytes uint64) {
	for _, file := range manifest.Files {
		size := uint64(file.Size)
		if file.Path == "config/config.yaml" {
			configBytes = saturatingAdd(configBytes, size)
			continue
		}
		dataBytes = saturatingAdd(dataBytes, size)
	}
	return dataBytes, configBytes
}

func sqliteSourceDiskBytes(path string) (uint64, error) {
	var total uint64
	for _, suffix := range []string{"", "-wal", "-journal"} {
		size, exists, err := regularFileSize(path + suffix)
		if err != nil {
			return 0, err
		}
		if !exists {
			continue
		}
		total = saturatingAdd(total, size)
	}
	return total, nil
}

func regularTreeDiskBytes(root string) (uint64, error) {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("desktop import source contains symbolic link: %s", root)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("desktop import source is not a directory: %s", root)
	}
	var total uint64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop import source contains symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("desktop import source is not a regular file: %s", path)
		}
		total = saturatingAdd(total, uint64(info.Size()))
		return nil
	})
	return total, err
}

func regularFileSize(path string) (uint64, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, false, fmt.Errorf("desktop data source is not a regular file: %s", path)
	}
	return uint64(info.Size()), true, nil
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func saturatingMultiply(value, multiplier uint64) uint64 {
	if value != 0 && multiplier > math.MaxUint64/value {
		return math.MaxUint64
	}
	return value * multiplier
}
