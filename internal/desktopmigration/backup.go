package desktopmigration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
	sqlite3 "github.com/mattn/go-sqlite3"
)

const BackupManifestSchemaVersion = 1

const (
	backupDirectoryMode = 0o700
	backupFileMode      = 0o600
)

type BackupFile struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

type BackupManifest struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Kind          string       `json:"kind"`
	FromVersion   string       `json:"from_version"`
	ToVersion     string       `json:"to_version"`
	CreatedAt     string       `json:"created_at"`
	TotalBytes    int64        `json:"total_bytes"`
	Files         []BackupFile `json:"files"`
}

type BackupResult struct {
	Directory string
	Manifest  BackupManifest
}

// CreateUpgradeBackup creates a verified, immutable backup point before a
// desktop version upgrade. SQLite files are copied through SQLite's online
// backup API so committed WAL frames are included in the snapshot.
func CreateUpgradeBackup(
	ctx context.Context,
	paths desktopruntime.Paths,
	fromVersion, toVersion string,
	createdAt time.Time,
) (BackupResult, error) {
	if ctx == nil {
		return BackupResult{}, errors.New("desktop backup context is required")
	}
	if err := ctx.Err(); err != nil {
		return BackupResult{}, err
	}
	fromVersion = strings.TrimSpace(fromVersion)
	toVersion = strings.TrimSpace(toVersion)
	if fromVersion == "" || toVersion == "" {
		return BackupResult{}, errors.New("desktop backup versions are required")
	}
	if createdAt.IsZero() {
		return BackupResult{}, errors.New("desktop backup creation time is required")
	}
	if err := validateBackupPaths(paths); err != nil {
		return BackupResult{}, err
	}
	if err := os.MkdirAll(paths.BackupsDir, backupDirectoryMode); err != nil {
		return BackupResult{}, fmt.Errorf("prepare desktop backup directory: %w", err)
	}
	if err := os.Chmod(paths.BackupsDir, backupDirectoryMode); err != nil {
		return BackupResult{}, fmt.Errorf("protect desktop backup directory: %w", err)
	}
	backupInfo, err := os.Lstat(paths.BackupsDir)
	if err != nil {
		return BackupResult{}, fmt.Errorf("inspect desktop backup directory: %w", err)
	}
	if !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
		return BackupResult{}, errors.New("desktop backup path is not a directory")
	}

	id, stagingDir, finalDir, err := reserveBackupDirectory(paths.BackupsDir, createdAt)
	if err != nil {
		return BackupResult{}, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	payloadDir := filepath.Join(stagingDir, "payload")
	if err := os.Mkdir(payloadDir, backupDirectoryMode); err != nil {
		return BackupResult{}, fmt.Errorf("prepare desktop backup payload: %w", err)
	}

	manifest := BackupManifest{
		SchemaVersion: BackupManifestSchemaVersion,
		ID:            id,
		Kind:          "upgrade",
		FromVersion:   fromVersion,
		ToVersion:     toVersion,
		CreatedAt:     createdAt.UTC().Format(time.RFC3339Nano),
		Files:         []BackupFile{},
	}
	if file, exists, err := backupRegularFile(ctx, paths.ConfigFile, filepath.Join(payloadDir, "config", "config.yaml"), "config/config.yaml"); err != nil {
		return BackupResult{}, err
	} else if exists {
		manifest.Files = append(manifest.Files, file)
	}

	databasePaths := map[string]struct{}{
		filepath.Clean(paths.DatabaseFile):          {},
		filepath.Clean(paths.KnowledgeDatabaseFile): {},
	}
	if err := filepath.WalkDir(paths.DataDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path = filepath.Clean(path)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop backup source contains symbolic link: %s", path)
		}
		if path == filepath.Clean(paths.BackupsDir) {
			return filepath.SkipDir
		}
		if path == filepath.Clean(paths.DataDir) {
			return nil
		}
		if entry.IsDir() {
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
		logicalPath, err := dataLogicalPath(paths.DataDir, path)
		if err != nil {
			return err
		}
		file, exists, err := backupRegularFile(ctx, path, filepath.Join(payloadDir, filepath.FromSlash(logicalPath)), logicalPath)
		if err != nil {
			return err
		}
		if exists {
			manifest.Files = append(manifest.Files, file)
		}
		return nil
	}); err != nil {
		return BackupResult{}, fmt.Errorf("copy desktop backup data: %w", err)
	}

	for _, databasePath := range []string{paths.DatabaseFile, paths.KnowledgeDatabaseFile} {
		logicalPath, err := dataLogicalPath(paths.DataDir, databasePath)
		if err != nil {
			return BackupResult{}, err
		}
		file, exists, err := backupSQLiteDatabase(ctx, databasePath, filepath.Join(payloadDir, filepath.FromSlash(logicalPath)), logicalPath)
		if err != nil {
			return BackupResult{}, err
		}
		if exists {
			manifest.Files = append(manifest.Files, file)
		}
	}

	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	for _, file := range manifest.Files {
		manifest.TotalBytes += file.Size
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupResult{}, fmt.Errorf("encode desktop backup manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	if err := writePrivateFile(filepath.Join(stagingDir, "manifest.json"), manifestData); err != nil {
		return BackupResult{}, fmt.Errorf("write desktop backup manifest: %w", err)
	}
	verified, err := VerifyBackup(stagingDir)
	if err != nil {
		return BackupResult{}, fmt.Errorf("verify desktop backup: %w", err)
	}
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return BackupResult{}, fmt.Errorf("publish desktop backup: %w", err)
	}
	removeStaging = false
	return BackupResult{Directory: finalDir, Manifest: verified}, nil
}

func VerifyBackup(directory string) (BackupManifest, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if !filepath.IsAbs(directory) {
		return BackupManifest{}, fmt.Errorf("desktop backup directory must be absolute: %q", directory)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return BackupManifest{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return BackupManifest{}, errors.New("desktop backup is not a directory")
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return BackupManifest{}, err
	}
	if !manifestInfo.Mode().IsRegular() {
		return BackupManifest{}, errors.New("desktop backup manifest is not a regular file")
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return BackupManifest{}, err
	}
	decoder := json.NewDecoder(manifestFile)
	decoder.DisallowUnknownFields()
	var manifest BackupManifest
	decodeErr := decoder.Decode(&manifest)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			decodeErr = errors.New("desktop backup manifest contains trailing data")
		}
	}
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		return BackupManifest{}, fmt.Errorf("decode desktop backup manifest: %w", decodeErr)
	}
	if closeErr != nil {
		return BackupManifest{}, closeErr
	}
	if err := validateBackupManifest(manifest); err != nil {
		return BackupManifest{}, err
	}
	payloadDir := filepath.Join(directory, "payload")
	if err := validateBackupPayload(payloadDir); err != nil {
		return BackupManifest{}, err
	}
	var totalBytes int64
	for _, file := range manifest.Files {
		path := filepath.Join(payloadDir, filepath.FromSlash(file.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("inspect desktop backup file %q: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return BackupManifest{}, fmt.Errorf("desktop backup file %q is not regular", file.Path)
		}
		if info.Size() != file.Size {
			return BackupManifest{}, fmt.Errorf("desktop backup file %q size mismatch", file.Path)
		}
		hash, err := hashFile(path)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("hash desktop backup file %q: %w", file.Path, err)
		}
		if hash != file.SHA256 {
			return BackupManifest{}, fmt.Errorf("desktop backup file %q checksum mismatch", file.Path)
		}
		totalBytes += file.Size
	}
	if totalBytes != manifest.TotalBytes {
		return BackupManifest{}, errors.New("desktop backup total size mismatch")
	}
	return manifest, nil
}

func validateBackupPayload(payloadDir string) error {
	info, err := os.Lstat(payloadDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("desktop backup payload is not a directory")
	}
	return filepath.WalkDir(payloadDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop backup payload contains symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("desktop backup payload contains special file: %s", path)
		}
		return nil
	})
}

func validateBackupPaths(paths desktopruntime.Paths) error {
	for name, path := range map[string]string{
		"data":               paths.DataDir,
		"config file":        paths.ConfigFile,
		"database":           paths.DatabaseFile,
		"knowledge database": paths.KnowledgeDatabaseFile,
		"backups":            paths.BackupsDir,
	} {
		if !filepath.IsAbs(strings.TrimSpace(path)) {
			return fmt.Errorf("desktop backup %s path must be absolute: %q", name, path)
		}
	}
	if !pathWithin(paths.DataDir, paths.BackupsDir) || filepath.Clean(paths.DataDir) == filepath.Clean(paths.BackupsDir) {
		return errors.New("desktop backup directory must be inside the data directory")
	}
	for _, databasePath := range []string{paths.DatabaseFile, paths.KnowledgeDatabaseFile} {
		if !pathWithin(paths.DataDir, databasePath) {
			return fmt.Errorf("desktop database is outside the data directory: %s", databasePath)
		}
	}
	return nil
}

func validateBackupManifest(manifest BackupManifest) error {
	if manifest.SchemaVersion != BackupManifestSchemaVersion {
		return fmt.Errorf("unsupported desktop backup manifest schema version: %d", manifest.SchemaVersion)
	}
	if manifest.ID == "" || filepath.Base(manifest.ID) != manifest.ID || strings.ContainsAny(manifest.ID, `/\`) {
		return errors.New("desktop backup manifest has invalid id")
	}
	if manifest.Kind != "upgrade" {
		return fmt.Errorf("unsupported desktop backup kind: %q", manifest.Kind)
	}
	if strings.TrimSpace(manifest.FromVersion) == "" || strings.TrimSpace(manifest.ToVersion) == "" {
		return errors.New("desktop backup manifest versions are required")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return errors.New("desktop backup manifest creation time is invalid")
	}
	if manifest.TotalBytes < 0 {
		return errors.New("desktop backup manifest total size is invalid")
	}
	previousPath := ""
	for _, file := range manifest.Files {
		if !fs.ValidPath(file.Path) || strings.Contains(file.Path, `\`) || (!strings.HasPrefix(file.Path, "config/") && !strings.HasPrefix(file.Path, "data/")) {
			return fmt.Errorf("desktop backup manifest contains invalid path %q", file.Path)
		}
		if strings.HasPrefix(file.Path, "config/") && file.Path != "config/config.yaml" {
			return fmt.Errorf("desktop backup manifest contains unsupported config path %q", file.Path)
		}
		if reservedBackupPayloadPath(file.Path) {
			return fmt.Errorf("desktop backup manifest contains reserved path %q", file.Path)
		}
		if previousPath != "" && file.Path <= previousPath {
			return errors.New("desktop backup manifest files are not uniquely sorted")
		}
		previousPath = file.Path
		if file.Kind != "file" && file.Kind != "sqlite" {
			return fmt.Errorf("desktop backup manifest contains invalid kind %q", file.Kind)
		}
		if len(file.SHA256) != sha256.Size*2 {
			return fmt.Errorf("desktop backup manifest contains invalid checksum for %q", file.Path)
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil || file.SHA256 != strings.ToLower(file.SHA256) {
			return fmt.Errorf("desktop backup manifest contains invalid checksum for %q", file.Path)
		}
		if file.Size < 0 || file.Mode > 0o777 {
			return fmt.Errorf("desktop backup manifest contains invalid metadata for %q", file.Path)
		}
	}
	return nil
}

func reservedBackupPayloadPath(path string) bool {
	return path == "data/backups" || strings.HasPrefix(path, "data/backups/") ||
		path == "data/upgrade-state.json" || path == "data/restore-state.json"
}

func reserveBackupDirectory(backupsDir string, createdAt time.Time) (id, stagingDir, finalDir string, err error) {
	baseID := "upgrade-" + createdAt.UTC().Format("20060102T150405.000000000Z")
	for sequence := 0; sequence < 100; sequence++ {
		id = baseID
		if sequence != 0 {
			id = fmt.Sprintf("%s-%02d", baseID, sequence)
		}
		finalDir = filepath.Join(backupsDir, id)
		if _, statErr := os.Lstat(finalDir); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return "", "", "", statErr
		}
		stagingDir = filepath.Join(backupsDir, "."+id+".pending")
		if mkdirErr := os.Mkdir(stagingDir, backupDirectoryMode); mkdirErr == nil {
			return id, stagingDir, finalDir, nil
		} else if !os.IsExist(mkdirErr) {
			return "", "", "", fmt.Errorf("reserve desktop backup directory: %w", mkdirErr)
		}
	}
	return "", "", "", errors.New("unable to reserve a unique desktop backup directory")
}

func backupRegularFile(ctx context.Context, source, destination, logicalPath string) (BackupFile, bool, error) {
	info, err := os.Lstat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return BackupFile{}, false, nil
		}
		return BackupFile{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return BackupFile{}, false, fmt.Errorf("desktop backup source is not a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), backupDirectoryMode); err != nil {
		return BackupFile{}, false, err
	}
	input, err := os.Open(source)
	if err != nil {
		return BackupFile{}, false, err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupFileMode)
	if err != nil {
		_ = input.Close()
		return BackupFile{}, false, err
	}
	hasher := sha256.New()
	_, copyErr := copyWithContext(ctx, io.MultiWriter(output, hasher), input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	inputCloseErr := input.Close()
	outputCloseErr := output.Close()
	if copyErr != nil {
		return BackupFile{}, false, copyErr
	}
	if inputCloseErr != nil {
		return BackupFile{}, false, inputCloseErr
	}
	if outputCloseErr != nil {
		return BackupFile{}, false, outputCloseErr
	}
	after, err := os.Lstat(source)
	if err != nil {
		return BackupFile{}, false, err
	}
	if !after.Mode().IsRegular() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return BackupFile{}, false, fmt.Errorf("desktop backup source changed while copying: %s", source)
	}
	return BackupFile{
		Path:   logicalPath,
		Kind:   "file",
		SHA256: hex.EncodeToString(hasher.Sum(nil)),
		Size:   info.Size(),
		Mode:   uint32(info.Mode().Perm()),
	}, true, nil
}

func backupSQLiteDatabase(ctx context.Context, source, destination, logicalPath string) (BackupFile, bool, error) {
	info, err := os.Lstat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return BackupFile{}, false, nil
		}
		return BackupFile{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return BackupFile{}, false, fmt.Errorf("desktop SQLite source is not a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), backupDirectoryMode); err != nil {
		return BackupFile{}, false, err
	}
	snapshotDir, err := os.MkdirTemp(filepath.Dir(destination), ".sqlite-source-*")
	if err != nil {
		return BackupFile{}, false, err
	}
	defer func() { _ = os.RemoveAll(snapshotDir) }()
	snapshotPath := filepath.Join(snapshotDir, filepath.Base(source))
	if err := snapshotSQLiteSource(ctx, source, snapshotPath); err != nil {
		return BackupFile{}, false, err
	}
	sourceDB, err := sql.Open("sqlite3", snapshotPath+"?_busy_timeout=5000")
	if err != nil {
		return BackupFile{}, false, err
	}
	sourceDB.SetMaxOpenConns(1)
	destinationDB, err := sql.Open("sqlite3", destination+"?_journal_mode=DELETE&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		_ = sourceDB.Close()
		return BackupFile{}, false, err
	}
	destinationDB.SetMaxOpenConns(1)
	closeDatabases := func() error {
		destinationErr := destinationDB.Close()
		sourceErr := sourceDB.Close()
		if destinationErr != nil {
			return destinationErr
		}
		return sourceErr
	}
	if _, err := sourceDB.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		_ = closeDatabases()
		return BackupFile{}, false, fmt.Errorf("open desktop SQLite backup source: %w", err)
	}
	sourceConn, err := sourceDB.Conn(ctx)
	if err != nil {
		_ = closeDatabases()
		return BackupFile{}, false, err
	}
	destinationConn, err := destinationDB.Conn(ctx)
	if err != nil {
		_ = sourceConn.Close()
		_ = closeDatabases()
		return BackupFile{}, false, err
	}
	backupErr := destinationConn.Raw(func(destinationDriver any) error {
		destinationSQLite, ok := destinationDriver.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("desktop SQLite destination driver is unavailable")
		}
		return sourceConn.Raw(func(sourceDriver any) error {
			sourceSQLite, ok := sourceDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("desktop SQLite source driver is unavailable")
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
			if err != nil {
				return err
			}
			closed := false
			defer func() {
				if !closed {
					_ = backup.Close()
				}
			}()
			for {
				done, err := backup.Step(256)
				if err != nil {
					return err
				}
				if done {
					if err := backup.Close(); err != nil {
						return err
					}
					closed = true
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
	})
	_ = destinationConn.Close()
	_ = sourceConn.Close()
	if backupErr != nil {
		_ = closeDatabases()
		return BackupFile{}, false, fmt.Errorf("copy desktop SQLite database %q: %w", logicalPath, backupErr)
	}
	var integrity string
	if err := destinationDB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		_ = closeDatabases()
		if err != nil {
			return BackupFile{}, false, fmt.Errorf("check desktop SQLite backup %q: %w", logicalPath, err)
		}
		return BackupFile{}, false, fmt.Errorf("check desktop SQLite backup %q: %s", logicalPath, integrity)
	}
	if err := closeDatabases(); err != nil {
		return BackupFile{}, false, err
	}
	if err := os.Chmod(destination, backupFileMode); err != nil {
		return BackupFile{}, false, err
	}
	if err := syncFile(destination); err != nil {
		return BackupFile{}, false, err
	}
	destinationInfo, err := os.Lstat(destination)
	if err != nil {
		return BackupFile{}, false, err
	}
	hash, err := hashFile(destination)
	if err != nil {
		return BackupFile{}, false, err
	}
	return BackupFile{
		Path:   logicalPath,
		Kind:   "sqlite",
		SHA256: hash,
		Size:   destinationInfo.Size(),
		Mode:   uint32(info.Mode().Perm()),
	}, true, nil
}

func snapshotSQLiteSource(ctx context.Context, source, destination string) error {
	type sourceState struct {
		path    string
		suffix  string
		size    int64
		modTime time.Time
	}
	states := make([]sourceState, 0, 3)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		path := source + suffix
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop SQLite source is not a regular file: %s", path)
		}
		states = append(states, sourceState{path: path, suffix: suffix, size: info.Size(), modTime: info.ModTime()})
	}
	for _, state := range states {
		if _, _, err := backupRegularFile(ctx, state.path, destination+state.suffix, "sqlite-source"); err != nil {
			return fmt.Errorf("stage desktop SQLite source %q: %w", state.path, err)
		}
	}
	for _, state := range states {
		info, err := os.Lstat(state.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != state.size || !info.ModTime().Equal(state.modTime) {
			return fmt.Errorf("desktop SQLite source changed while staging: %s", state.path)
		}
	}
	return nil
}

func dataLogicalPath(dataDir, path string) (string, error) {
	if !pathWithin(dataDir, path) || filepath.Clean(dataDir) == filepath.Clean(path) {
		return "", fmt.Errorf("desktop backup path is outside data directory: %s", path)
	}
	relative, err := filepath.Rel(dataDir, path)
	if err != nil {
		return "", err
	}
	return "data/" + filepath.ToSlash(relative), nil
}

func pathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func isSQLiteSidecar(path string, databases map[string]struct{}) bool {
	for databasePath := range databases {
		if path == databasePath+"-wal" || path == databasePath+"-shm" || path == databasePath+"-journal" {
			return true
		}
	}
	return false
}

func writePrivateFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}
