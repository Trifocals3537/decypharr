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

func TestSetDefaultsUsesLoopbackBindAddress(t *testing.T) {
	cfg := &Config{}
	if err := cfg.setDefaultsForPath(t.TempDir(), false); err != nil {
		t.Fatalf("setDefaultsForPath() error = %v", err)
	}
	if cfg.BindAddress != DefaultBindAddress {
		t.Fatalf("BindAddress = %q, want %q", cfg.BindAddress, DefaultBindAddress)
	}
}

func TestSetDefaultsPreservesExplicitBindAddress(t *testing.T) {
	cfg := &Config{BindAddress: "0.0.0.0"}
	if err := cfg.setDefaultsForPath(t.TempDir(), false); err != nil {
		t.Fatalf("setDefaultsForPath() error = %v", err)
	}
	if cfg.BindAddress != "0.0.0.0" {
		t.Fatalf("BindAddress = %q, want explicit override", cfg.BindAddress)
	}
}

func TestFirstLoadAppliesBindAddressEnvironmentOverride(t *testing.T) {
	previousPath := GetMainPath()
	configDir := t.TempDir()
	SetConfigPath(configDir)
	t.Cleanup(func() {
		SetConfigPath(previousPath)
	})
	t.Setenv("DECYPHARR_BIND_ADDRESS", "0.0.0.0")

	cfg := &Config{}
	if err := cfg.loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.BindAddress != "0.0.0.0" {
		t.Fatalf("runtime BindAddress = %q, want environment override", cfg.BindAddress)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if !bytes.Contains(data, []byte(`"bind_address": "127.0.0.1"`)) {
		t.Fatalf("generated config did not retain the safe native bind default: %s", data)
	}
}
