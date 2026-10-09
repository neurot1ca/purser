package pluginroot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesExecutableRootOutsideRepository(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root)
	executable := filepath.Join(root, "bin", "purser-hook-test")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, nil, 0700); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	got := resolve("", func() (string, error) { return executable, nil }, func() (string, error) { return cwd, nil })
	if got != root {
		t.Fatalf("resolve() = %q, want %q", got, root)
	}
}

func TestResolveConfiguredRootWins(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured")
	got := resolve(configured, func() (string, error) { return "", os.ErrNotExist }, func() (string, error) { return "", os.ErrNotExist })
	if got != configured {
		t.Fatalf("resolve() = %q, want configured root %q", got, configured)
	}
}

func TestResolveWalksFromWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root)
	cwd := filepath.Join(root, "nested", "repo")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}

	got := resolve("", func() (string, error) { return "", os.ErrNotExist }, func() (string, error) { return cwd, nil })
	if got != root {
		t.Fatalf("resolve() = %q, want %q", got, root)
	}
}

func writeConfig(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(configPath))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
}
