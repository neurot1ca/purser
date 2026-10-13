package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveGoModCacheKeepsExistingValue(t *testing.T) {
	got := resolveGoModCache("custom/cache", "workspace", "home")
	if got != "custom/cache" {
		t.Fatalf("resolveGoModCache() = %q, want existing value", got)
	}
}

func TestResolveGoModCacheUsesFirstGOPATHEntry(t *testing.T) {
	goPath := filepath.Join("first", "gopath") + string(os.PathListSeparator) + filepath.Join("second", "gopath")
	want := filepath.Join("first", "gopath", "pkg", "mod")
	if got := resolveGoModCache("", goPath, "home"); got != want {
		t.Fatalf("resolveGoModCache() = %q, want %q", got, want)
	}
}

func TestResolveGoModCacheUsesHomeWhenGOPATHIsMissing(t *testing.T) {
	want := filepath.Join("original-home", "go", "pkg", "mod")
	if got := resolveGoModCache("", "", "original-home"); got != want {
		t.Fatalf("resolveGoModCache() = %q, want %q", got, want)
	}
}
