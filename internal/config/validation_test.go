package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadForValidationIsReadOnly(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	original := []byte(`{
  "use_auth": true,
  "debrids": [
    {
      "name": "realdebrid",
      "api_key": "test-key"
    }
  ]
}`)
	if err := os.WriteFile(configPath, original, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadForValidation(configDir)
	if err != nil {
		t.Fatalf("LoadForValidation() error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if want := filepath.Join(configDir, "downloads"); cfg.DownloadFolder != want {
		t.Fatalf("DownloadFolder = %q, want %q", cfg.DownloadFolder, want)
	}
	if want := filepath.Join(configDir, "usenet", "streams"); cfg.Usenet.DiskBufferPath != want {
		t.Fatalf("Usenet.DiskBufferPath = %q, want %q", cfg.Usenet.DiskBufferPath, want)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("LoadForValidation changed config.json")
	}
	if _, err := os.Stat(filepath.Join(configDir, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth.json was created during validation: %v", err)
	}
	if entries, err := os.ReadDir(configDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("validation created unexpected files: %v", entries)
	}
}

func TestLoadForValidationRejectsInvalidJSON(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"debrids":`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadForValidation(configDir); err == nil {
		t.Fatal("LoadForValidation() error = nil, want invalid JSON error")
	}
}

func TestLoadForValidationRequiresExistingConfig(t *testing.T) {
	configDir := t.TempDir()

	_, err := LoadForValidation(configDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadForValidation() error = %v, want os.ErrNotExist", err)
	}
}
