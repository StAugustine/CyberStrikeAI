package logger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloseReleasesOwnedLogFileAndIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "desktop.log")
	log := New("info", path)
	log.Info("desktop logger close test")
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	renamed := filepath.Join(directory, "desktop-closed.log")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatalf("rename closed log file: %v", err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("remove closed log file: %v", err)
	}
}
