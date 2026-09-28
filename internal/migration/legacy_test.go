package migration

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMigrateCopiesVerifiesAndRenamesState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, ".decypharr")
	target := filepath.Join(root, ".tessarr")
	mustWriteFile(t, filepath.Join(source, "config.json"), []byte(`{"name":"Decypharr remains valid user data"}`), 0o600)
	mustWriteFile(t, filepath.Join(source, "logs", "decypharr.log"), []byte("ready\n"), 0o640)
	mustWriteFile(t, filepath.Join(source, ".decypharr-cache-owner"), []byte("owner"), 0o600)
	mustWriteFile(t, filepath.Join(source, ".decypharr-torrent-quarantine-123"), []byte("held"), 0o600)
	mustWriteFile(t, filepath.Join(source, ".decypharr-strm-root"), []byte("root"), 0o600)

	result, err := Migrate(Options{Source: source, Target: target})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if result.Files != 5 || result.RenamedEntries != 4 || result.AlreadyMigrated || result.DryRun {
		t.Fatalf("Migrate() result = %+v", result)
	}
	assertFileContent(t, filepath.Join(source, "logs", "decypharr.log"), "ready\n")
	assertFileContent(t, filepath.Join(target, "logs", "tessarr.log"), "ready\n")
	assertFileContent(t, filepath.Join(target, ".tessarr-cache-owner"), "owner")
	assertFileContent(t, filepath.Join(target, ".tessarr-torrent-quarantine-123"), "held")
	assertFileContent(t, filepath.Join(target, ".tessarr-strm-root"), "root")
	assertFileContent(t, filepath.Join(target, "config.json"), `{"name":"Decypharr remains valid user data"}`)
	if _, err := os.Stat(filepath.Join(target, receiptName)); err != nil {
		t.Fatalf("migration receipt missing: %v", err)
	}

	again, err := Migrate(Options{Source: source, Target: target})
	if err != nil {
		t.Fatalf("idempotent Migrate() error = %v", err)
	}
	if !again.AlreadyMigrated {
		t.Fatalf("idempotent Migrate() result = %+v", again)
	}
}

func TestMigrateDryRunMakesNoChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	target := filepath.Join(root, "new")
	mustWriteFile(t, filepath.Join(source, "decypharr.log"), []byte("log"), 0o600)

	result, err := Migrate(Options{Source: source, Target: target, DryRun: true})
	if err != nil {
		t.Fatalf("Migrate(dry-run) error = %v", err)
	}
	if !result.DryRun || result.Files != 1 || result.RenamedEntries != 1 {
		t.Fatalf("Migrate(dry-run) result = %+v", result)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run created target: %v", err)
	}
}

func TestMigrateRejectsTransformedCollision(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	mustWriteFile(t, filepath.Join(source, ".decypharr-cache-owner"), []byte("old"), 0o600)
	mustWriteFile(t, filepath.Join(source, ".tessarr-cache-owner"), []byte("new"), 0o600)

	_, err := Migrate(Options{Source: source, Target: filepath.Join(root, "new")})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("Migrate() error = %v, want collision", err)
	}
}

func TestMigrateRejectsReservedReceiptSourceName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{receiptName, ".TESSARR-MIGRATION.JSON"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "old")
			target := filepath.Join(root, "new")
			mustWriteFile(t, filepath.Join(source, name), []byte("user data"), 0o600)

			_, err := Migrate(Options{Source: source, Target: target})
			if err == nil || !strings.Contains(err.Error(), "reserves target name") {
				t.Fatalf("Migrate() error = %v, want reserved receipt rejection", err)
			}
			if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
				t.Fatalf("reserved-name rejection created target: %v", statErr)
			}
		})
	}
}

func TestMigrateRejectsSymlinkedExistingReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows test users may not have symlink permission")
	}
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	target := filepath.Join(root, "new")
	mustWriteFile(t, filepath.Join(source, "config.json"), []byte("{}"), 0o600)
	if _, err := Migrate(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(target, receiptName)
	receiptData, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-receipt.json")
	mustWriteFile(t, outside, receiptData, 0o600)
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, receiptPath); err != nil {
		t.Fatal(err)
	}

	_, err = Migrate(Options{Source: source, Target: target})
	if err == nil || !strings.Contains(err.Error(), "receipt is not a regular file") {
		t.Fatalf("Migrate() error = %v, want symlinked receipt rejection", err)
	}
}

func TestMigrateRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows test users may not have symlink permission")
	}
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	mustWriteFile(t, filepath.Join(source, "outside"), []byte("secret"), 0o600)
	if err := os.Symlink(filepath.Join(source, "outside"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}

	_, err := Migrate(Options{Source: source, Target: filepath.Join(root, "new")})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Migrate() error = %v, want symlink rejection", err)
	}
}

func TestMigrateRejectsSymlinkedTargetParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows test users may not have symlink permission")
	}
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	realParent := filepath.Join(root, "real")
	mustWriteFile(t, filepath.Join(source, "config.json"), []byte("{}"), 0o600)
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}

	_, err := Migrate(Options{Source: source, Target: filepath.Join(linkedParent, "new")})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Migrate() error = %v, want target-parent symlink rejection", err)
	}
}

func TestMigrateRejectsNestedRootsAndUnverifiedTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "old")
	mustWriteFile(t, filepath.Join(source, "config.json"), []byte("{}"), 0o600)

	if _, err := Migrate(Options{Source: source, Target: filepath.Join(source, "new")}); err == nil {
		t.Fatal("Migrate() accepted nested target")
	}

	target := filepath.Join(root, "new")
	mustWriteFile(t, filepath.Join(target, "existing"), []byte("preserve"), 0o600)
	if _, err := Migrate(Options{Source: source, Target: target}); err == nil {
		t.Fatal("Migrate() accepted unverified existing target")
	}
	assertFileContent(t, filepath.Join(target, "existing"), "preserve")
}

func mustWriteFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, content, want)
	}
}
