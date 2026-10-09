package pluginroot

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if !samePath(t, got, root) {
		t.Fatalf("resolve() = %q, want %q", got, root)
	}
}

func TestResolveConfiguredRootWins(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured")
	if err := os.MkdirAll(configured, 0700); err != nil {
		t.Fatal(err)
	}
	got := resolve(configured, func() (string, error) { return "", os.ErrNotExist }, func() (string, error) { return "", os.ErrNotExist })
	if !samePath(t, got, configured) {
		t.Fatalf("resolve() = %q, want configured root %q", got, configured)
	}
}

func TestResolveRejectsExecutableRootWithoutConfig(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "purser-hook-test")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, nil, 0700); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	got := resolve("", func() (string, error) { return executable, nil }, func() (string, error) { return cwd, nil })
	if got != "" {
		t.Fatalf("resolve() = %q, want empty for invalid executable and cwd roots", got)
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
	if !samePath(t, got, root) {
		t.Fatalf("resolve() = %q, want %q", got, root)
	}
}

func samePath(t *testing.T, got, want string) bool {
	t.Helper()
	canonical := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", path, err)
		}
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(resolved, `\\?\UNC\`) {
				resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
			} else {
				resolved = strings.TrimPrefix(resolved, `\\?\`)
			}
		}
		return filepath.Clean(resolved)
	}
	got, want = canonical(got), canonical(want)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(got, want)
	}
	return got == want
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
