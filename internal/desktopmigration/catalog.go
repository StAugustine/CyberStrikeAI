package desktopmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cyberstrike-ai/internal/desktopruntime"
)

type BackupSummary struct {
	ID          string `json:"id"`
	Kind        string `json:"kind,omitempty"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	TotalBytes  int64  `json:"total_bytes,omitempty"`
	FileCount   int    `json:"file_count,omitempty"`
	Valid       bool   `json:"valid"`
	Protected   bool   `json:"protected"`
	Retained    bool   `json:"retained"`
	Deletable   bool   `json:"deletable"`
	Error       string `json:"error,omitempty"`
}

func ListBackups(paths desktopruntime.Paths) ([]BackupSummary, error) {
	if !filepath.IsAbs(paths.BackupsDir) {
		return nil, fmt.Errorf("desktop backup directory must be absolute: %q", paths.BackupsDir)
	}
	entries, err := os.ReadDir(paths.BackupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupSummary{}, nil
		}
		return nil, err
	}
	pending, pendingExists, pendingErr := LoadUpgradeState(paths.UpgradeStateFile)
	if pendingErr != nil {
		return nil, pendingErr
	}
	pendingImport, pendingImportExists, pendingImportErr := LoadImportState(paths.ImportStateFile)
	if pendingImportErr != nil {
		return nil, pendingImportErr
	}
	summaries := make([]BackupSummary, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		summary := BackupSummary{
			ID: entry.Name(),
			Protected: (pendingExists && pending.BackupID == entry.Name()) ||
				(pendingImportExists && pendingImport.BackupID == entry.Name()),
		}
		if !entry.IsDir() {
			summary.Error = "backup entry is not a directory"
			summaries = append(summaries, summary)
			continue
		}
		manifest, err := VerifyBackup(filepath.Join(paths.BackupsDir, entry.Name()))
		if err != nil {
			summary.Error = err.Error()
			summaries = append(summaries, summary)
			continue
		}
		if manifest.ID != entry.Name() {
			summary.Error = "backup directory does not match manifest id"
			summaries = append(summaries, summary)
			continue
		}
		summary.ID = manifest.ID
		summary.Kind = manifest.Kind
		summary.FromVersion = manifest.FromVersion
		summary.ToVersion = manifest.ToVersion
		summary.CreatedAt = manifest.CreatedAt
		summary.TotalBytes = manifest.TotalBytes
		summary.FileCount = len(manifest.Files)
		summary.Valid = true
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Valid != summaries[j].Valid {
			return summaries[i].Valid
		}
		left, leftErr := time.Parse(time.RFC3339Nano, summaries[i].CreatedAt)
		right, rightErr := time.Parse(time.RFC3339Nano, summaries[j].CreatedAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		return summaries[i].ID < summaries[j].ID
	})
	validIndex := 0
	for index := range summaries {
		if !summaries[index].Valid {
			continue
		}
		summaries[index].Retained = validIndex < 2 || summaries[index].Protected
		summaries[index].Deletable = !summaries[index].Retained
		validIndex++
	}
	return summaries, nil
}
