//go:build windows

package contextio

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileIdentity(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first")
	secondPath := filepath.Join(dir, "second")
	if err := os.WriteFile(firstPath, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}

	first := fileIdentity(firstPath, nil)
	if first == nil {
		t.Fatal("identity for existing file is nil")
	}
	if again := fileIdentity(firstPath, nil); !reflect.DeepEqual(first, again) {
		t.Fatalf("same file identities differ: %v != %v", first, again)
	}
	if second := fileIdentity(secondPath, nil); second == nil || reflect.DeepEqual(first, second) {
		t.Fatalf("different file identity is nil or equal: %v and %v", first, second)
	}
	if missing := fileIdentity(filepath.Join(dir, "missing"), nil); missing != nil {
		t.Fatalf("missing path identity = %v, want nil", missing)
	}
}
