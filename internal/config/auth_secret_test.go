package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	json "github.com/bytedance/sonic"
)

func TestSecretKeyGeneratesAndPersistsPerInstallSecret(t *testing.T) {
	t.Setenv("DECYPHARR_SECRET_KEY", "")

	oldConfigPath := configPath
	configPath = t.TempDir()
	t.Cleanup(func() {
		configPath = oldConfigPath
	})

	cfg := &Config{}
	secret := cfg.SecretKey()
	if len(secret) != 64 {
		t.Fatalf("SecretKey() length = %d, want 64 hex characters", len(secret))
	}

	data, err := os.ReadFile(cfg.AuthFile())
	if err != nil {
		t.Fatal(err)
	}
	var persisted Auth
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SessionSecret != secret {
		t.Fatal("persisted session secret does not match the active key")
	}

	reloaded := (&Config{}).SecretKey()
	if reloaded != secret {
		t.Fatal("SecretKey() did not reuse the persisted per-install key")
	}
}

func TestSecretKeyHonorsEnvironmentOverride(t *testing.T) {
	t.Setenv("DECYPHARR_SECRET_KEY", "operator-provided-secret")

	cfg := &Config{}
	if got := cfg.SecretKey(); got != "operator-provided-secret" {
		t.Fatalf("SecretKey() = %q, want environment override", got)
	}
}

func TestLoadConfigRejectsMalformedAuthWithoutOverwriting(t *testing.T) {
	tests := map[string][]byte{
		"truncated object": []byte(`{"username":"admin"`),
		"null root":        []byte(`null`),
	}
	for name, original := range tests {
		t.Run(name, func(t *testing.T) {
			configDir := useTemporaryConfigPath(t)
			if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}

			authPath := filepath.Join(configDir, "auth.json")
			if err := os.WriteFile(authPath, original, 0600); err != nil {
				t.Fatal(err)
			}

			cfg := &Config{}
			err := cfg.loadConfig()
			if err == nil {
				t.Fatal("loadConfig() succeeded with malformed auth.json")
			}
			if !strings.Contains(err.Error(), "parse auth config") {
				t.Fatalf("loadConfig() error = %q, want auth parse error", err)
			}
			if cfg.Auth != nil {
				t.Fatal("malformed auth.json was replaced with an in-memory auth object")
			}

			got, readErr := os.ReadFile(authPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != string(original) {
				t.Fatalf("auth.json = %q, want original malformed data %q", got, original)
			}
		})
	}
}

func TestLoadConfigRejectsUnreadableAuthPath(t *testing.T) {
	configDir := useTemporaryConfigPath(t)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	authPath := filepath.Join(configDir, "auth.json")
	if err := os.Mkdir(authPath, 0700); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	err := cfg.loadConfig()
	if err == nil {
		t.Fatal("loadConfig() succeeded when auth.json could not be read as a file")
	}
	if !strings.Contains(err.Error(), "read auth config") {
		t.Fatalf("loadConfig() error = %q, want auth read error", err)
	}
	if cfg.Auth != nil {
		t.Fatal("unreadable auth.json was replaced with an in-memory auth object")
	}
	info, statErr := os.Stat(authPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !info.IsDir() {
		t.Fatal("unreadable auth path was replaced")
	}
}

func TestSecretKeyFailsWhenNewSecretCannotBePersisted(t *testing.T) {
	configDir := useTemporaryConfigPath(t)
	cfg := &Config{}
	if _, err := cfg.loadAuth(); err != nil {
		t.Fatalf("initialize missing auth: %v", err)
	}

	if err := os.Remove(configDir); err != nil {
		t.Fatal(err)
	}
	const blocker = "preserve this file"
	if err := os.WriteFile(configDir, []byte(blocker), 0600); err != nil {
		t.Fatal(err)
	}

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_ = cfg.SecretKey()
	}()
	if recovered == nil {
		t.Fatal("SecretKey() succeeded when auth.json could not be persisted")
	}
	if !strings.Contains(recovered.(error).Error(), "persist session secret") {
		t.Fatalf("SecretKey() panic = %v, want persistence error", recovered)
	}

	got, err := os.ReadFile(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != blocker {
		t.Fatalf("blocking file = %q, want %q", got, blocker)
	}
}

func useTemporaryConfigPath(t *testing.T) string {
	t.Helper()

	oldConfigPath := configPath
	configDir := t.TempDir()
	configPath = configDir
	t.Cleanup(func() {
		configPath = oldConfigPath
	})
	return configDir
}
