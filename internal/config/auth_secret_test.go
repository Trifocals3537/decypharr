package config

import (
	"os"
	"testing"

	json "github.com/bytedance/sonic"
)

func TestSecretKeyGeneratesAndPersistsPerInstallSecret(t *testing.T) {
	t.Setenv("CONVEYARR_SECRET_KEY", "")
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
	t.Setenv("CONVEYARR_SECRET_KEY", "operator-provided-secret")
	t.Setenv("DECYPHARR_SECRET_KEY", "")

	cfg := &Config{}
	if got := cfg.SecretKey(); got != "operator-provided-secret" {
		t.Fatalf("SecretKey() = %q, want environment override", got)
	}
}
