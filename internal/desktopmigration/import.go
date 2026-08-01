package desktopmigration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/desktopcredentials"
	"cyberstrike-ai/internal/desktopruntime"
	"gopkg.in/yaml.v3"
)

const ImportStateSchemaVersion = 1
const ImportReportSchemaVersion = 1

const legacyImportMaximumConfigBytes = 8 * 1024 * 1024

var legacyImportMinimumVersion = semanticVersion{core: [3]uint64{1, 7, 8}}
var legacyImportMaximumVersion = semanticVersion{core: [3]uint64{1, 8, 0}}

type ImportReport struct {
	SchemaVersion            int      `json:"schema_version"`
	SourceName               string   `json:"source_name"`
	SourceVersion            string   `json:"source_version"`
	TargetVersion            string   `json:"target_version"`
	BackupID                 string   `json:"backup_id"`
	FileCount                int      `json:"file_count"`
	TotalBytes               int64    `json:"total_bytes"`
	ImportedSections         []string `json:"imported_sections"`
	PlaintextCredentialPaths []string `json:"plaintext_credential_paths"`
	ReferenceCredentialPaths []string `json:"reference_credential_paths"`
	ExcludedCapabilities     []string `json:"excluded_capabilities"`
	RewrittenConfigPaths     []string `json:"rewritten_config_paths"`
	IgnoredConfigPaths       []string `json:"ignored_config_paths"`
	RemovedUserAccounts      int      `json:"removed_user_accounts"`
	Warnings                 []string `json:"warnings"`
}

type ImportState struct {
	SchemaVersion int          `json:"schema_version"`
	Status        string       `json:"status"`
	BackupID      string       `json:"backup_id"`
	PreparedAt    string       `json:"prepared_at"`
	Report        ImportReport `json:"report"`
}

type ImportSession struct {
	paths desktopruntime.Paths
	state ImportState
}

type ImportCommitResult struct {
	BackupID         string `json:"backup_id"`
	RollbackBackupID string `json:"rollback_backup_id"`
	SourceVersion    string `json:"source_version"`
	TargetVersion    string `json:"target_version"`
	RestoredFiles    int    `json:"restored_files"`
}

func (s *ImportSession) State() ImportState {
	if s == nil {
		return ImportState{}
	}
	state := s.state
	state.Report = cloneImportReport(state.Report)
	return state
}

func (s *ImportSession) Report() ImportReport {
	if s == nil {
		return ImportReport{}
	}
	return cloneImportReport(s.state.Report)
}

// LoadPendingImportSession reconstructs an import prepared by an earlier
// maintenance process without needing the original source directory.
func LoadPendingImportSession(paths desktopruntime.Paths) (*ImportSession, bool, error) {
	if err := validateImportPaths(paths); err != nil {
		return nil, false, err
	}
	state, exists, err := LoadImportState(paths.ImportStateFile)
	if err != nil || !exists {
		return nil, exists, err
	}
	if err := verifyPendingImportBackup(paths, state); err != nil {
		return nil, false, err
	}
	return &ImportSession{paths: paths, state: state}, true, nil
}

func CancelPendingImport(paths desktopruntime.Paths) (bool, error) {
	if err := validateImportPaths(paths); err != nil {
		return false, err
	}
	state, exists, err := LoadImportState(paths.ImportStateFile)
	if err != nil || !exists {
		return false, err
	}
	if err := (&ImportSession{paths: paths, state: state}).Cancel(); err != nil {
		return false, err
	}
	return true, nil
}

// CommitOffline creates a recovery point for the current desktop data, then
// applies the prepared import through the recoverable restore transaction.
// The desktop core must be stopped so no live database handles remain open.
func (s *ImportSession) CommitOffline(ctx context.Context, startedAt time.Time) (ImportCommitResult, error) {
	if s == nil {
		return ImportCommitResult{}, errors.New("desktop import session is required")
	}
	if ctx == nil {
		return ImportCommitResult{}, errors.New("desktop import context is required")
	}
	if err := ctx.Err(); err != nil {
		return ImportCommitResult{}, err
	}
	if startedAt.IsZero() {
		return ImportCommitResult{}, errors.New("desktop import commit time is required")
	}
	current, exists, err := LoadImportState(s.paths.ImportStateFile)
	if err != nil {
		return ImportCommitResult{}, err
	}
	if !exists || !reflect.DeepEqual(current, s.state) {
		return ImportCommitResult{}, errors.New("desktop import state changed before confirmation")
	}
	if err := verifyPendingImportBackup(s.paths, current); err != nil {
		return ImportCommitResult{}, err
	}
	rollback, err := CreateUpgradeBackup(ctx, s.paths, current.Report.TargetVersion, current.Report.TargetVersion, startedAt)
	if err != nil {
		return ImportCommitResult{}, fmt.Errorf("create pre-import desktop recovery point: %w", err)
	}
	restored, err := RestoreBackup(ctx, s.paths, current.BackupID)
	if err != nil {
		return ImportCommitResult{RollbackBackupID: rollback.Manifest.ID}, err
	}
	return ImportCommitResult{
		BackupID:         current.BackupID,
		RollbackBackupID: rollback.Manifest.ID,
		SourceVersion:    current.Report.SourceVersion,
		TargetVersion:    current.Report.TargetVersion,
		RestoredFiles:    restored.Restored,
	}, nil
}

// Cancel discards a prepared import snapshot. Existing desktop data and
// unrelated recovery points are never touched.
func (s *ImportSession) Cancel() error {
	if s == nil {
		return errors.New("desktop import session is required")
	}
	current, exists, err := LoadImportState(s.paths.ImportStateFile)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !reflect.DeepEqual(current, s.state) {
		return errors.New("desktop import state changed before cancellation")
	}
	backupDirectory := filepath.Join(s.paths.BackupsDir, current.BackupID)
	if info, err := os.Lstat(backupDirectory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("pending desktop import backup is not a real directory")
		}
		if err := os.RemoveAll(backupDirectory); err != nil {
			return fmt.Errorf("discard pending desktop import backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.paths.ImportStateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove desktop import state: %w", err)
	}
	return nil
}

type legacyImportDirectory struct {
	section     string
	source      string
	destination string
}

type legacyImportSource struct {
	root               string
	config             config.Config
	database           string
	knowledgeDatabase  string
	directories        []legacyImportDirectory
	credentialReport   desktopcredentials.ConfigCredentialInspection
	ignoredConfigPaths []string
}

type legacyImportTreeFile struct {
	relative string
	size     int64
	mode     fs.FileMode
	modTime  time.Time
}

// PrepareLegacyImport validates and snapshots an existing repository-style
// instance into an immutable desktop recovery point. It does not replace live
// desktop configuration or data; a later explicit confirmation performs that
// switch through the restore transaction.
func PrepareLegacyImport(
	ctx context.Context,
	paths desktopruntime.Paths,
	sourceRoot string,
	targetVersion string,
	preparedAt time.Time,
) (*ImportSession, error) {
	if ctx == nil {
		return nil, errors.New("desktop import context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if preparedAt.IsZero() {
		return nil, errors.New("desktop import preparation time is required")
	}
	targetVersion = strings.TrimSpace(targetVersion)
	if targetVersion == "" {
		return nil, errors.New("desktop import target version is required")
	}
	if err := validateImportPaths(paths); err != nil {
		return nil, err
	}
	if _, exists, err := LoadImportState(paths.ImportStateFile); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("desktop import is already pending")
	}
	if _, exists, err := LoadRestoreState(paths.RestoreStateFile); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("desktop restore must finish before import")
	}
	if _, exists, err := LoadUpgradeState(paths.UpgradeStateFile); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("desktop upgrade must finish before import")
	}

	source, err := inspectLegacyImportSource(sourceRoot, paths)
	if err != nil {
		return nil, err
	}
	id, stagingDirectory, finalDirectory, err := reserveBackupDirectoryWithPrefix(paths.BackupsDir, "import", preparedAt)
	if err != nil {
		return nil, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(stagingDirectory)
		}
	}()
	payloadDirectory := filepath.Join(stagingDirectory, "payload")
	if err := os.Mkdir(payloadDirectory, backupDirectoryMode); err != nil {
		return nil, fmt.Errorf("prepare desktop import payload: %w", err)
	}

	manifest := BackupManifest{
		SchemaVersion: BackupManifestSchemaVersion,
		ID:            id,
		Kind:          "import",
		FromVersion:   source.config.Version,
		ToVersion:     targetVersion,
		CreatedAt:     preparedAt.UTC().Format(time.RFC3339Nano),
		Files:         []BackupFile{},
	}
	sections := make(map[string]struct{})
	configFile, err := writeLegacyImportConfig(filepath.Join(payloadDirectory, "config", "config.yaml"), &source.config)
	if err != nil {
		return nil, err
	}
	manifest.Files = append(manifest.Files, configFile)
	sections["config"] = struct{}{}

	databaseFile, exists, err := backupSQLiteDatabase(ctx, source.database, filepath.Join(payloadDirectory, "data", "databases", "conversations.db"), "data/databases/conversations.db")
	if err != nil {
		return nil, fmt.Errorf("snapshot legacy conversation database: %w", err)
	}
	if !exists {
		return nil, errors.New("legacy conversation database does not exist")
	}
	removedUsers, err := normalizeImportedDesktopDatabase(filepath.Join(payloadDirectory, filepath.FromSlash(databaseFile.Path)))
	if err != nil {
		return nil, err
	}
	databaseFile, err = refreshImportedBackupFile(filepath.Join(payloadDirectory, filepath.FromSlash(databaseFile.Path)), databaseFile)
	if err != nil {
		return nil, err
	}
	manifest.Files = append(manifest.Files, databaseFile)
	sections["database"] = struct{}{}

	if source.knowledgeDatabase != "" && filepath.Clean(source.knowledgeDatabase) != filepath.Clean(source.database) {
		knowledgeFile, exists, err := backupSQLiteDatabase(ctx, source.knowledgeDatabase, filepath.Join(payloadDirectory, "data", "databases", "knowledge.db"), "data/databases/knowledge.db")
		if err != nil {
			return nil, fmt.Errorf("snapshot legacy knowledge database: %w", err)
		}
		if !exists {
			return nil, errors.New("configured legacy knowledge database does not exist")
		}
		manifest.Files = append(manifest.Files, knowledgeFile)
		sections["knowledge_database"] = struct{}{}
	}

	for _, directory := range source.directories {
		files, err := copyLegacyImportDirectory(ctx, directory.source, payloadDirectory, directory.destination)
		if err != nil {
			return nil, fmt.Errorf("snapshot legacy import section %s: %w", directory.section, err)
		}
		if len(files) == 0 {
			continue
		}
		manifest.Files = append(manifest.Files, files...)
		sections[directory.section] = struct{}{}
	}

	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	for index, file := range manifest.Files {
		if index != 0 && manifest.Files[index-1].Path == file.Path {
			return nil, fmt.Errorf("legacy import maps multiple files to %q", file.Path)
		}
		manifest.TotalBytes += file.Size
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode desktop import manifest: %w", err)
	}
	if err := writePrivateFile(filepath.Join(stagingDirectory, "manifest.json"), append(manifestData, '\n')); err != nil {
		return nil, fmt.Errorf("write desktop import manifest: %w", err)
	}
	verified, err := VerifyBackup(stagingDirectory)
	if err != nil {
		return nil, fmt.Errorf("verify desktop import snapshot: %w", err)
	}
	if err := os.Rename(stagingDirectory, finalDirectory); err != nil {
		return nil, fmt.Errorf("publish desktop import snapshot: %w", err)
	}
	removeStaging = false

	report := ImportReport{
		SchemaVersion:            ImportReportSchemaVersion,
		SourceName:               filepath.Base(source.root),
		SourceVersion:            source.config.Version,
		TargetVersion:            targetVersion,
		BackupID:                 id,
		FileCount:                len(verified.Files),
		TotalBytes:               verified.TotalBytes,
		ImportedSections:         sortedRestoreEntrySet(sections),
		PlaintextCredentialPaths: append([]string{}, source.credentialReport.PlaintextPaths...),
		ReferenceCredentialPaths: append([]string{}, source.credentialReport.ReferencePaths...),
		ExcludedCapabilities: []string{
			"c2",
			"local_terminal",
			"multi_user_rbac",
			"remote_service",
			"robots",
			"webshell",
		},
		RewrittenConfigPaths: []string{
			"agent.system_prompt_path",
			"agent.workspace_root_dir",
			"agents_dir",
			"database.knowledge_db_path",
			"database.path",
			"knowledge.base_path",
			"log.output",
			"multi_agent.eino_middleware.checkpoint_dir",
			"multi_agent.eino_middleware.reduction_root_dir",
			"roles_dir",
			"security.tools_dir",
			"server",
			"skills_dir",
		},
		IgnoredConfigPaths:  append([]string{}, source.ignoredConfigPaths...),
		RemovedUserAccounts: removedUsers,
		Warnings:            []string{"The source instance must remain stopped until import confirmation completes."},
	}
	if len(report.PlaintextCredentialPaths) != 0 {
		report.Warnings = append(report.Warnings, "Plaintext credentials require explicit migration into the operating system credential store on first imported startup.")
	}
	if len(report.ReferenceCredentialPaths) != 0 {
		report.Warnings = append(report.Warnings, "Referenced credentials must already exist in this operating system credential store.")
	}
	state := ImportState{
		SchemaVersion: ImportStateSchemaVersion,
		Status:        "pending",
		BackupID:      id,
		PreparedAt:    preparedAt.UTC().Format(time.RFC3339Nano),
		Report:        report,
	}
	stateData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		_ = os.RemoveAll(finalDirectory)
		return nil, fmt.Errorf("encode desktop import state: %w", err)
	}
	if err := writePrivateStateAtomically(paths.ImportStateFile, append(stateData, '\n')); err != nil {
		_ = os.RemoveAll(finalDirectory)
		return nil, fmt.Errorf("write desktop import state: %w", err)
	}
	return &ImportSession{paths: paths, state: state}, nil
}

func LoadImportState(path string) (ImportState, bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return ImportState{}, false, fmt.Errorf("desktop import state path must be absolute: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ImportState{}, false, nil
		}
		return ImportState{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ImportState{}, false, errors.New("desktop import state is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return ImportState{}, false, err
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state ImportState
	decodeErr := decoder.Decode(&state)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			decodeErr = errors.New("desktop import state contains trailing data")
		}
	}
	closeErr := file.Close()
	if decodeErr != nil {
		return ImportState{}, false, fmt.Errorf("decode desktop import state: %w", decodeErr)
	}
	if closeErr != nil {
		return ImportState{}, false, closeErr
	}
	if err := validateImportState(state); err != nil {
		return ImportState{}, false, err
	}
	return state, true, nil
}

func inspectLegacyImportSource(sourceRoot string, paths desktopruntime.Paths) (legacyImportSource, error) {
	sourceRoot = filepath.Clean(strings.TrimSpace(sourceRoot))
	if !filepath.IsAbs(sourceRoot) {
		return legacyImportSource{}, fmt.Errorf("legacy import source must be absolute: %q", sourceRoot)
	}
	for _, ownedRoot := range []string{paths.DataDir, paths.ConfigDir, paths.CacheDir, paths.LogDir, paths.TempDir} {
		if pathWithin(sourceRoot, ownedRoot) || pathWithin(ownedRoot, sourceRoot) {
			return legacyImportSource{}, errors.New("legacy import source overlaps desktop-owned storage")
		}
	}
	info, err := os.Lstat(sourceRoot)
	if err != nil {
		return legacyImportSource{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return legacyImportSource{}, errors.New("legacy import source is not a real directory")
	}
	configPath := filepath.Join(sourceRoot, "config.yaml")
	configData, err := readLegacyImportConfig(configPath)
	if err != nil {
		return legacyImportSource{}, err
	}
	var cfg config.Config
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		return legacyImportSource{}, fmt.Errorf("parse legacy import config: %w", err)
	}
	cfg.Version = strings.TrimSpace(cfg.Version)
	version, ok := parseSemanticVersion(cfg.Version)
	if !ok || compareSemanticVersions(version, legacyImportMinimumVersion) < 0 || compareSemanticVersions(version, legacyImportMaximumVersion) >= 0 {
		return legacyImportSource{}, fmt.Errorf("legacy config version %q is unsupported; supported range is v1.7.8 through v1.7.x", cfg.Version)
	}
	credentialReport, err := desktopcredentials.InspectConfigCredentials(&cfg)
	if err != nil {
		return legacyImportSource{}, err
	}

	database, _, err := legacyImportSourcePath(sourceRoot, cfg.Database.Path, "data/conversations.db", "database.path")
	if err != nil {
		return legacyImportSource{}, err
	}
	knowledgeDatabase := ""
	if strings.TrimSpace(cfg.Database.KnowledgeDBPath) != "" {
		knowledgeDatabase, _, err = legacyImportSourcePath(sourceRoot, cfg.Database.KnowledgeDBPath, "", "database.knowledge_db_path")
		if err != nil {
			return legacyImportSource{}, err
		}
	}

	directories := make([]legacyImportDirectory, 0, 10)
	addDirectory := func(section, value, fallback, destination string) error {
		path, _, err := legacyImportSourcePath(sourceRoot, value, fallback, section)
		if err != nil {
			return err
		}
		directories = append(directories, legacyImportDirectory{section: section, source: path, destination: destination})
		return nil
	}
	for _, item := range []struct {
		section, value, fallback, destination string
	}{
		{"tools", cfg.Security.ToolsDir, "tools", "data/resources/tools"},
		{"roles", cfg.RolesDir, "roles", "data/resources/roles"},
		{"skills", cfg.SkillsDir, "skills", "data/resources/skills"},
		{"agents", cfg.AgentsDir, "agents", "data/resources/agents"},
		{"knowledge_base", cfg.Knowledge.BasePath, "knowledge_base", "data/resources/knowledge_base"},
		{"chat_uploads", "chat_uploads", "", "data/chat_uploads"},
		{"conversation_artifacts", filepath.ToSlash(filepath.Join(filepath.Dir(relativeToLegacyRoot(sourceRoot, database)), "conversation_artifacts")), "", "data/databases/conversation_artifacts"},
		{"workflow_checkpoints", "data/workflow-checkpoints", "", "data/checkpoints/workflows"},
	} {
		if err := addDirectory(item.section, item.value, item.fallback, item.destination); err != nil {
			return legacyImportSource{}, err
		}
	}
	if strings.TrimSpace(cfg.Agent.WorkspaceRootDir) != "" {
		if err := addDirectory("workspaces", cfg.Agent.WorkspaceRootDir, "", "data/workspaces"); err != nil {
			return legacyImportSource{}, err
		}
	}
	if strings.TrimSpace(cfg.MultiAgent.EinoMiddleware.CheckpointDir) != "" {
		if err := addDirectory("agent_checkpoints", cfg.MultiAgent.EinoMiddleware.CheckpointDir, "", "data/checkpoints/agents"); err != nil {
			return legacyImportSource{}, err
		}
	}
	ignoredConfigPaths := []string{}
	ignoredPathValues := map[string]string{
		"agent.system_prompt_path":                       cfg.Agent.SystemPromptPath,
		"multi_agent.eino_middleware.reduction_root_dir": cfg.MultiAgent.EinoMiddleware.ReductionRootDir,
		"server.tls_cert_path":                           cfg.Server.TLSCertPath,
		"server.tls_key_path":                            cfg.Server.TLSKeyPath,
	}
	logOutput := strings.TrimSpace(cfg.Log.Output)
	if logOutput != "" && logOutput != "stdout" && logOutput != "stderr" {
		ignoredPathValues["log.output"] = logOutput
	}
	for field, value := range ignoredPathValues {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, _, err := legacyImportSourcePath(sourceRoot, value, "", field); err != nil {
			return legacyImportSource{}, err
		}
		ignoredConfigPaths = append(ignoredConfigPaths, field)
	}
	if strings.TrimSpace(cfg.MCP.AuthHeader) != "" {
		ignoredConfigPaths = append(ignoredConfigPaths, "mcp.auth_header")
	}
	if strings.TrimSpace(cfg.MCP.AuthHeaderValue) != "" {
		ignoredConfigPaths = append(ignoredConfigPaths, "mcp.auth_header_value")
	}
	sort.Strings(ignoredConfigPaths)

	disabled := false
	desktopruntime.ApplyScope(&cfg)
	cfg.C2.Enabled = &disabled
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 0
	cfg.Server.CORSAllowedOrigins = nil
	cfg.Server.TLSEnabled = false
	cfg.Server.TLSCertPath = ""
	cfg.Server.TLSKeyPath = ""
	cfg.Server.TLSAutoSelfSign = false
	cfg.Server.TLSHTTPRedirect = nil
	cfg.MCP.AuthHeader = ""
	cfg.MCP.AuthHeaderValue = ""
	cfg.MCP.AllowGlobalAccess = false
	cfg.Log.Output = "stdout"
	cfg.Database.Path = "data/conversations.db"
	if knowledgeDatabase == "" || filepath.Clean(knowledgeDatabase) == filepath.Clean(database) {
		cfg.Database.KnowledgeDBPath = ""
	} else {
		cfg.Database.KnowledgeDBPath = "data/knowledge.db"
	}
	cfg.Security.ToolsDir = "tools"
	cfg.RolesDir = "roles"
	cfg.SkillsDir = "skills"
	cfg.AgentsDir = "agents"
	cfg.Knowledge.BasePath = "knowledge_base"
	cfg.Agent.WorkspaceRootDir = ""
	cfg.Agent.SystemPromptPath = ""
	cfg.MultiAgent.EinoMiddleware.CheckpointDir = ""
	cfg.MultiAgent.EinoMiddleware.ReductionRootDir = ""

	return legacyImportSource{
		root:               sourceRoot,
		config:             cfg,
		database:           database,
		knowledgeDatabase:  knowledgeDatabase,
		directories:        directories,
		credentialReport:   credentialReport,
		ignoredConfigPaths: ignoredConfigPaths,
	}, nil
}

func readLegacyImportConfig(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect legacy import config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("legacy import config is not a regular file")
	}
	if info.Size() > legacyImportMaximumConfigBytes {
		return nil, errors.New("legacy import config is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, legacyImportMaximumConfigBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > legacyImportMaximumConfigBytes {
		return nil, errors.New("legacy import config is too large")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("legacy import config changed while reading")
	}
	return data, nil
}

func legacyImportSourcePath(root, value, fallback, field string) (string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" || filepath.IsAbs(value) {
		return "", "", fmt.Errorf("legacy import %s must be a relative path inside the selected source", field)
	}
	relative := filepath.Clean(filepath.FromSlash(value))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("legacy import %s escapes the selected source", field)
	}
	path := filepath.Join(root, relative)
	if !pathWithin(root, path) {
		return "", "", fmt.Errorf("legacy import %s escapes the selected source", field)
	}
	if err := rejectLegacyImportSymlinkComponents(root, path); err != nil {
		return "", "", fmt.Errorf("legacy import %s: %w", field, err)
	}
	return path, filepath.ToSlash(relative), nil
}

func rejectLegacyImportSymlinkComponents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source path contains symbolic link: %s", filepath.ToSlash(relative))
		}
	}
	return nil
}

func relativeToLegacyRoot(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "data/conversations.db"
	}
	return relative
}

func writeLegacyImportConfig(path string, cfg *config.Config) (BackupFile, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupFile{}, fmt.Errorf("encode legacy import config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), backupDirectoryMode); err != nil {
		return BackupFile{}, err
	}
	if err := writePrivateFile(path, data); err != nil {
		return BackupFile{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return BackupFile{}, err
	}
	hash, err := hashFile(path)
	if err != nil {
		return BackupFile{}, err
	}
	return BackupFile{Path: "config/config.yaml", Kind: "file", SHA256: hash, Size: info.Size(), Mode: uint32(info.Mode().Perm())}, nil
}

func copyLegacyImportDirectory(ctx context.Context, source, payloadRoot, destination string) ([]BackupFile, error) {
	files, exists, err := scanLegacyImportDirectory(source)
	if err != nil || !exists {
		return nil, err
	}
	result := make([]BackupFile, 0, len(files))
	for _, sourceFile := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		logicalPath := filepath.ToSlash(filepath.Join(filepath.FromSlash(destination), sourceFile.relative))
		file, exists, err := backupRegularFile(ctx, filepath.Join(source, sourceFile.relative), filepath.Join(payloadRoot, filepath.FromSlash(logicalPath)), logicalPath)
		if err != nil {
			return nil, err
		}
		if exists {
			result = append(result, file)
		}
	}
	after, exists, err := scanLegacyImportDirectory(source)
	if err != nil || !exists {
		return nil, errors.New("legacy import directory disappeared while copying")
	}
	if !legacyImportTreesEqual(files, after) {
		return nil, errors.New("legacy import directory changed while copying")
	}
	return result, nil
}

func scanLegacyImportDirectory(root string) ([]legacyImportTreeFile, bool, error) {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []legacyImportTreeFile{}, false, nil
		}
		return nil, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("legacy import section is not a real directory")
	}
	files := []legacyImportTreeFile{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("legacy import section contains symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("legacy import section contains special file: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, legacyImportTreeFile{relative: relative, size: info.Size(), mode: info.Mode(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].relative < files[j].relative })
	return files, true, nil
}

func legacyImportTreesEqual(left, right []legacyImportTreeFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].relative != right[index].relative || left[index].size != right[index].size || left[index].mode != right[index].mode || !left[index].modTime.Equal(right[index].modTime) {
			return false
		}
	}
	return true
}

func normalizeImportedDesktopDatabase(path string) (int, error) {
	database, err := sql.Open("sqlite3", path+"?_foreign_keys=1&_busy_timeout=5000")
	if err != nil {
		return 0, err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	var tableCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'rbac_users'`).Scan(&tableCount); err != nil {
		return 0, err
	}
	if tableCount == 0 {
		return 0, nil
	}
	var removed int
	if err := database.QueryRow(`SELECT COUNT(*) FROM rbac_users WHERE username <> 'admin' OR is_builtin <> 1`).Scan(&removed); err != nil {
		return 0, err
	}
	transaction, err := database.Begin()
	if err != nil {
		return 0, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = transaction.Rollback()
		}
	}()
	if _, err := transaction.Exec(`DELETE FROM rbac_users WHERE username <> 'admin' OR is_builtin <> 1`); err != nil {
		return 0, fmt.Errorf("remove imported multi-user accounts: %w", err)
	}
	if _, err := transaction.Exec(`DELETE FROM rbac_roles WHERE is_system = 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return 0, fmt.Errorf("remove imported custom RBAC roles: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	rollback = false
	var integrity string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("normalized imported database integrity check failed: %s", integrity)
	}
	return removed, nil
}

func refreshImportedBackupFile(path string, file BackupFile) (BackupFile, error) {
	if err := os.Chmod(path, backupFileMode); err != nil {
		return BackupFile{}, err
	}
	if err := syncFile(path); err != nil {
		return BackupFile{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return BackupFile{}, err
	}
	hash, err := hashFile(path)
	if err != nil {
		return BackupFile{}, err
	}
	file.Size = info.Size()
	file.SHA256 = hash
	file.Mode = uint32(info.Mode().Perm())
	return file, nil
}

func validateImportPaths(paths desktopruntime.Paths) error {
	if !filepath.IsAbs(paths.ImportStateFile) || !pathWithin(paths.DataDir, paths.ImportStateFile) || filepath.Clean(paths.ImportStateFile) == filepath.Clean(paths.DataDir) {
		return errors.New("desktop import state file must be inside the data directory")
	}
	return validateRestorePaths(paths)
}

func validateImportState(state ImportState) error {
	if state.SchemaVersion != ImportStateSchemaVersion {
		return fmt.Errorf("unsupported desktop import state schema version: %d", state.SchemaVersion)
	}
	if state.Status != "pending" {
		return fmt.Errorf("unsupported desktop import status: %q", state.Status)
	}
	if state.BackupID == "" || filepath.Base(state.BackupID) != state.BackupID || !strings.HasPrefix(state.BackupID, "import-") || strings.ContainsAny(state.BackupID, `/\\`) {
		return errors.New("desktop import state backup id is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.PreparedAt); err != nil {
		return errors.New("desktop import state preparation time is invalid")
	}
	if state.Report.SchemaVersion != ImportReportSchemaVersion || state.Report.BackupID != state.BackupID || strings.TrimSpace(state.Report.SourceVersion) == "" || strings.TrimSpace(state.Report.TargetVersion) == "" {
		return errors.New("desktop import report does not match pending state")
	}
	return nil
}

func verifyPendingImportBackup(paths desktopruntime.Paths, state ImportState) error {
	if err := validateImportState(state); err != nil {
		return err
	}
	manifest, err := VerifyBackup(filepath.Join(paths.BackupsDir, state.BackupID))
	if err != nil {
		return fmt.Errorf("verify pending desktop import backup: %w", err)
	}
	if manifest.ID != state.BackupID || manifest.Kind != "import" || manifest.FromVersion != state.Report.SourceVersion || manifest.ToVersion != state.Report.TargetVersion || len(manifest.Files) != state.Report.FileCount || manifest.TotalBytes != state.Report.TotalBytes {
		return errors.New("pending desktop import backup does not match its report")
	}
	return nil
}

func cloneImportReport(report ImportReport) ImportReport {
	report.ImportedSections = append([]string{}, report.ImportedSections...)
	report.PlaintextCredentialPaths = append([]string{}, report.PlaintextCredentialPaths...)
	report.ReferenceCredentialPaths = append([]string{}, report.ReferenceCredentialPaths...)
	report.ExcludedCapabilities = append([]string{}, report.ExcludedCapabilities...)
	report.RewrittenConfigPaths = append([]string{}, report.RewrittenConfigPaths...)
	report.IgnoredConfigPaths = append([]string{}, report.IgnoredConfigPaths...)
	report.Warnings = append([]string{}, report.Warnings...)
	return report
}
