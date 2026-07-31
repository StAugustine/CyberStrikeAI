package desktopruntime

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func TestInstallResourcesFirstInstallAndUnchangedRestart(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "resources")
	state := filepath.Join(root, "state", "resource-state.json")
	source, manifest := testResourceSource("v1", map[string]string{
		"agents/recon.md": "recon v1",
		"tools/nmap.yaml": "nmap v1",
	})

	result, err := InstallResources(source, manifest, target, state)
	if err != nil {
		t.Fatalf("InstallResources first: %v", err)
	}
	if !reflect.DeepEqual(result.Installed, []string{"agents/recon.md", "tools/nmap.yaml"}) {
		t.Fatalf("installed = %#v", result.Installed)
	}
	assertFileContent(t, filepath.Join(target, "agents", "recon.md"), "recon v1")

	restarted, err := InstallResources(source, manifest, target, state)
	if err != nil {
		t.Fatalf("InstallResources restart: %v", err)
	}
	if !reflect.DeepEqual(restarted.Unchanged, []string{"agents/recon.md", "tools/nmap.yaml"}) {
		t.Fatalf("unchanged = %#v", restarted.Unchanged)
	}
}

func TestInstallResourcesUpdatesOnlyUnmodifiedFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "resources")
	state := filepath.Join(root, "resource-state.json")
	v1Source, v1Manifest := testResourceSource("v1", map[string]string{
		"agents/recon.md": "recon v1",
		"tools/nmap.yaml": "nmap v1",
	})
	if _, err := InstallResources(v1Source, v1Manifest, target, state); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "agents", "recon.md"), []byte("user recon"), 0o600); err != nil {
		t.Fatal(err)
	}

	v2Source, v2Manifest := testResourceSource("v2", map[string]string{
		"agents/recon.md":   "recon v2",
		"skills/a/SKILL.md": "skill v2",
		"tools/nmap.yaml":   "nmap v2",
	})
	result, err := InstallResources(v2Source, v2Manifest, target, state)
	if err != nil {
		t.Fatalf("install v2: %v", err)
	}
	if !reflect.DeepEqual(result.Updated, []string{"tools/nmap.yaml"}) {
		t.Fatalf("updated = %#v", result.Updated)
	}
	if !reflect.DeepEqual(result.Installed, []string{"skills/a/SKILL.md"}) {
		t.Fatalf("installed = %#v", result.Installed)
	}
	if !reflect.DeepEqual(result.Conflicts, []string{"agents/recon.md"}) {
		t.Fatalf("conflicts = %#v", result.Conflicts)
	}
	assertFileContent(t, filepath.Join(target, "agents", "recon.md"), "user recon")
	assertFileContent(t, filepath.Join(target, "tools", "nmap.yaml"), "nmap v2")
}

func TestInstallResourcesPreservesModifiedFileWithoutUpstreamChange(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "resources")
	state := filepath.Join(root, "resource-state.json")
	source, manifest := testResourceSource("v1", map[string]string{"roles/default.yaml": "default"})
	if _, err := InstallResources(source, manifest, target, state); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	path := filepath.Join(target, "roles", "default.yaml")
	if err := os.WriteFile(path, []byte("user default"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.AppVersion = "v1.0.1"
	result, err := InstallResources(source, manifest, target, state)
	if err != nil {
		t.Fatalf("install unchanged upstream: %v", err)
	}
	if !reflect.DeepEqual(result.Preserved, []string{"roles/default.yaml"}) || len(result.Conflicts) != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertFileContent(t, path, "user default")
}

func TestInstallResourcesReportsOrphanWithoutDeletingIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "resources")
	state := filepath.Join(root, "resource-state.json")
	v1Source, v1Manifest := testResourceSource("v1", map[string]string{"tools/old.yaml": "old"})
	if _, err := InstallResources(v1Source, v1Manifest, target, state); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	v2Source, v2Manifest := testResourceSource("v2", map[string]string{})
	result, err := InstallResources(v2Source, v2Manifest, target, state)
	if err != nil {
		t.Fatalf("install v2: %v", err)
	}
	if !reflect.DeepEqual(result.Orphaned, []string{"tools/old.yaml"}) {
		t.Fatalf("orphaned = %#v", result.Orphaned)
	}
	assertFileContent(t, filepath.Join(target, "tools", "old.yaml"), "old")
}

func TestInstallResourcesRejectsHashMismatchBeforeWriting(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "resources")
	state := filepath.Join(root, "resource-state.json")
	source, manifest := testResourceSource("v1", map[string]string{"tools/nmap.yaml": "nmap"})
	manifest.Files[0].SHA256 = strings.Repeat("0", 64)

	_, err := InstallResources(source, manifest, target, state)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("InstallResources error = %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target was written before source verification: %v", statErr)
	}
}

func TestParseResourceManifestRejectsUnsafePath(t *testing.T) {
	data := `{"schema_version":1,"app_version":"v1","files":[{"path":"../secret","sha256":"` + strings.Repeat("0", 64) + `"}]}`
	_, err := ParseResourceManifest([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("ParseResourceManifest error = %v", err)
	}
}

func TestDesktopResourceManifestMatchesWorkspaceDefaults(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	workspaceDir := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	manifestData, err := os.ReadFile(filepath.Join(workspaceDir, "desktop", "resources", "manifest.json"))
	if err != nil {
		t.Fatalf("read desktop resource manifest: %v", err)
	}
	manifest, err := ParseResourceManifest(manifestData)
	if err != nil {
		t.Fatalf("ParseResourceManifest: %v", err)
	}
	if len(manifest.Files) < 100 {
		t.Fatalf("manifest contains only %d files; default resource set is incomplete", len(manifest.Files))
	}
	if _, err := prepareResourceSource(os.DirFS(workspaceDir), manifest); err != nil {
		t.Fatalf("verify workspace resources: %v", err)
	}
}

func testResourceSource(version string, files map[string]string) (fs.FS, ResourceManifest) {
	mapFS := fstest.MapFS{}
	manifest := ResourceManifest{SchemaVersion: resourceManifestSchemaVersion, AppVersion: version}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sortStrings(paths)
	for _, path := range paths {
		content := []byte(files[path])
		mapFS[path] = &fstest.MapFile{Data: content, Mode: 0o600}
		manifest.Files = append(manifest.Files, ResourceFile{Path: path, SHA256: hashBytes(content)})
	}
	return mapFS, manifest
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(content) != expected {
		t.Fatalf("%s content = %q, want %q", path, content, expected)
	}
}
