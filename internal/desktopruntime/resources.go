package desktopruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const resourceManifestSchemaVersion = 1

type ResourceManifest struct {
	SchemaVersion int            `json:"schema_version"`
	AppVersion    string         `json:"app_version"`
	Files         []ResourceFile `json:"files"`
}

type ResourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ResourceInstallResult struct {
	Installed []string
	Updated   []string
	Preserved []string
	Conflicts []string
	Orphaned  []string
	Unchanged []string
}

type resourceState struct {
	SchemaVersion int               `json:"schema_version"`
	AppVersion    string            `json:"app_version"`
	Files         map[string]string `json:"files"`
	Conflicts     []string          `json:"conflicts,omitempty"`
	Orphaned      []string          `json:"orphaned,omitempty"`
}

type preparedResource struct {
	manifest ResourceFile
	content  []byte
}

func ParseResourceManifest(data []byte) (ResourceManifest, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest ResourceManifest
	if err := decoder.Decode(&manifest); err != nil {
		return ResourceManifest{}, fmt.Errorf("decode resource manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return ResourceManifest{}, err
	}
	return manifest, nil
}

func (m ResourceManifest) Validate() error {
	if m.SchemaVersion != resourceManifestSchemaVersion {
		return fmt.Errorf("unsupported resource manifest schema version: %d", m.SchemaVersion)
	}
	if strings.TrimSpace(m.AppVersion) == "" {
		return errors.New("resource manifest app_version is required")
	}
	seen := make(map[string]struct{}, len(m.Files))
	for index, file := range m.Files {
		if !validResourcePath(file.Path) {
			return fmt.Errorf("resource manifest file %d has invalid path %q", index, file.Path)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("resource manifest contains duplicate path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if !validSHA256(file.SHA256) {
			return fmt.Errorf("resource manifest file %q has invalid sha256", file.Path)
		}
	}
	return nil
}

func validResourcePath(path string) bool {
	return path != "." && path == filepath.ToSlash(path) && fs.ValidPath(path)
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// InstallResources applies a verified resource manifest to the writable
// desktop resource directory. Files changed by the user are never overwritten.
func InstallResources(source fs.FS, manifest ResourceManifest, targetDir, statePath string) (ResourceInstallResult, error) {
	if source == nil {
		return ResourceInstallResult{}, errors.New("resource source filesystem is required")
	}
	if err := manifest.Validate(); err != nil {
		return ResourceInstallResult{}, err
	}
	if !filepath.IsAbs(targetDir) {
		return ResourceInstallResult{}, fmt.Errorf("resource target directory must be absolute: %q", targetDir)
	}
	if !filepath.IsAbs(statePath) {
		return ResourceInstallResult{}, fmt.Errorf("resource state path must be absolute: %q", statePath)
	}

	prepared, err := prepareResourceSource(source, manifest)
	if err != nil {
		return ResourceInstallResult{}, err
	}
	previous, err := loadResourceState(statePath)
	if err != nil {
		return ResourceInstallResult{}, err
	}
	if err := prepareWritableDirectory(targetDir); err != nil {
		return ResourceInstallResult{}, fmt.Errorf("prepare resource target: %w", err)
	}
	if err := prepareWritableDirectory(filepath.Dir(statePath)); err != nil {
		return ResourceInstallResult{}, fmt.Errorf("prepare resource state directory: %w", err)
	}

	result := ResourceInstallResult{}
	nextHashes := make(map[string]string, len(prepared))
	for _, resource := range prepared {
		path := resource.manifest.Path
		nextHashes[path] = resource.manifest.SHA256
		targetPath := filepath.Join(targetDir, filepath.FromSlash(path))
		currentHash, exists, hashErr := hashExistingRegularFile(targetPath)
		if hashErr != nil {
			return ResourceInstallResult{}, fmt.Errorf("inspect resource %q: %w", path, hashErr)
		}
		if !exists {
			if err := replaceResourceFile(targetPath, resource.content); err != nil {
				return ResourceInstallResult{}, fmt.Errorf("install resource %q: %w", path, err)
			}
			result.Installed = append(result.Installed, path)
			continue
		}
		if currentHash == resource.manifest.SHA256 {
			result.Unchanged = append(result.Unchanged, path)
			continue
		}
		previousHash, wasManaged := previous.Files[path]
		if wasManaged && currentHash == previousHash {
			if err := replaceResourceFile(targetPath, resource.content); err != nil {
				return ResourceInstallResult{}, fmt.Errorf("update resource %q: %w", path, err)
			}
			result.Updated = append(result.Updated, path)
			continue
		}
		result.Preserved = append(result.Preserved, path)
		if !wasManaged || previousHash != resource.manifest.SHA256 {
			result.Conflicts = append(result.Conflicts, path)
		}
	}

	for path := range previous.Files {
		if _, exists := nextHashes[path]; !exists {
			result.Orphaned = append(result.Orphaned, path)
		}
	}
	sortResourceResult(&result)
	nextState := resourceState{
		SchemaVersion: resourceManifestSchemaVersion,
		AppVersion:    manifest.AppVersion,
		Files:         nextHashes,
		Conflicts:     result.Conflicts,
		Orphaned:      result.Orphaned,
	}
	stateData, err := json.MarshalIndent(nextState, "", "  ")
	if err != nil {
		return ResourceInstallResult{}, fmt.Errorf("encode resource state: %w", err)
	}
	stateData = append(stateData, '\n')
	if err := replaceResourceFile(statePath, stateData); err != nil {
		return ResourceInstallResult{}, fmt.Errorf("write resource state: %w", err)
	}
	return result, nil
}

func prepareResourceSource(source fs.FS, manifest ResourceManifest) ([]preparedResource, error) {
	prepared := make([]preparedResource, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		content, err := fs.ReadFile(source, file.Path)
		if err != nil {
			return nil, fmt.Errorf("read bundled resource %q: %w", file.Path, err)
		}
		if actual := hashBytes(content); actual != file.SHA256 {
			return nil, fmt.Errorf("bundled resource %q hash mismatch: got %s, want %s", file.Path, actual, file.SHA256)
		}
		prepared = append(prepared, preparedResource{manifest: file, content: content})
	}
	return prepared, nil
}

func loadResourceState(path string) (resourceState, error) {
	state := resourceState{Files: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return resourceState{}, fmt.Errorf("read resource state: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return resourceState{}, fmt.Errorf("decode resource state: %w", err)
	}
	if state.SchemaVersion != resourceManifestSchemaVersion {
		return resourceState{}, fmt.Errorf("unsupported resource state schema version: %d", state.SchemaVersion)
	}
	if strings.TrimSpace(state.AppVersion) == "" {
		return resourceState{}, errors.New("resource state app_version is required")
	}
	if state.Files == nil {
		state.Files = map[string]string{}
	}
	for path, hash := range state.Files {
		if !validResourcePath(path) || !validSHA256(hash) {
			return resourceState{}, fmt.Errorf("resource state contains invalid file entry %q", path)
		}
	}
	return state, nil
}

// InstalledResourceVersion returns the version last published by a complete
// resource installation. A missing state file identifies a fresh install.
func InstalledResourceVersion(path string) (version string, exists bool, err error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("resource state path must be absolute: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect resource state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("resource state is not a regular file")
	}
	state, err := loadResourceState(path)
	if err != nil {
		return "", false, err
	}
	return state.AppVersion, true, nil
}

func hashExistingRegularFile(path string) (hash string, exists bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", true, errors.New("target is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", true, err
	}
	return hashBytes(data), true, nil
}

func replaceResourceFile(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, privateDirectoryMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".desktop-resource-*")
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
	if err := temporary.Chmod(0o600); err != nil {
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
		backupPath := temporaryPath + ".previous"
		if err := os.Rename(path, backupPath); err != nil {
			return err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			_ = os.Rename(backupPath, path)
			return err
		}
		removeTemporary = false
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("remove replaced resource backup: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func sortResourceResult(result *ResourceInstallResult) {
	sort.Strings(result.Installed)
	sort.Strings(result.Updated)
	sort.Strings(result.Preserved)
	sort.Strings(result.Conflicts)
	sort.Strings(result.Orphaned)
	sort.Strings(result.Unchanged)
}
