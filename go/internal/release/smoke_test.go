package release

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistributionSmokeValidFixture(t *testing.T) {
	withStubbedDistributionRuntime(t)
	artifactDir := writeDistributionFixture(t, true, false, false)
	if err := DistributionSmoke(artifactDir); err != nil {
		t.Fatalf("valid distribution rejected: %v", err)
	}
}

func TestDistributionSmokeRejectsChecksumMismatch(t *testing.T) {
	withStubbedDistributionRuntime(t)
	artifactDir := writeDistributionFixture(t, true, true, false)
	err := DistributionSmoke(artifactDir)
	if err == nil || !strings.Contains(err.Error(), "archive checksum mismatch") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
}

func TestDistributionSmokeReportsMissingBinary(t *testing.T) {
	withStubbedDistributionRuntime(t)
	artifactDir := writeDistributionFixture(t, false, false, false)
	err := DistributionSmoke(artifactDir)
	if err == nil || !strings.Contains(err.Error(), "host binary missing from archive") {
		t.Fatalf("missing host binary was not reported: %v", err)
	}
}

func TestDistributionSmokeRejectsManifestDigestMismatch(t *testing.T) {
	withStubbedDistributionRuntime(t)
	artifactDir := writeDistributionFixture(t, true, false, true)
	err := DistributionSmoke(artifactDir)
	if err == nil || !strings.Contains(err.Error(), "host binary digest is not covered by release metadata") {
		t.Fatalf("manifest digest mismatch accepted: %v", err)
	}
}

func withStubbedDistributionRuntime(t *testing.T) {
	t.Helper()
	previous := verifyDistributionRuntime
	verifyDistributionRuntime = func(string, string, string) error { return nil }
	t.Cleanup(func() { verifyDistributionRuntime = previous })
}

func writeDistributionFixture(t *testing.T, includeBinary, badChecksum, badManifest bool) string {
	t.Helper()
	artifactDir := t.TempDir()
	_, _, name, err := HostTarget()
	if err != nil {
		t.Fatal(err)
	}
	binaryName := "bin/" + name
	binary := []byte("fixture executable")
	archive := filepath.Join(artifactDir, "purser-1.2.3.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	writeZip := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		h := &zip.FileHeader{Name: path}
		h.SetMode(mode)
		entry, e := w.CreateHeader(h)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = entry.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if includeBinary {
		writeZip(binaryName, binary, 0700)
	}
	writeZip("hooks/hooks.json", []byte(`{"modules":["register.ts"]}`), 0644)
	writeZip("hooks/register.ts", []byte("classic.SessionStart classic.UserPromptSubmit $.plugin.root}/bin/ from './mod/enforcement.js'"), 0644)
	writeZip("hooks/mod/enforcement.ts", []byte("classic.PreToolUse $.plugin.root}/bin/"), 0644)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digest(binary)
	if badManifest {
		manifestDigest = strings.Repeat("0", 64)
	}
	manifest := Manifest{BinaryDigests: map[string]string{binaryName: manifestDigest}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(artifactDir, "release-manifest.json"), manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}
	archiveDigest := digest(archiveBytes)
	if badChecksum {
		archiveDigest = strings.Repeat("0", 64)
	}
	sums := archiveDigest + "  " + filepath.Base(archive) + "\n" + digest(binary) + "  " + binaryName + "\n"
	if err = os.WriteFile(filepath.Join(artifactDir, "SHA256SUMS"), []byte(sums), 0644); err != nil {
		t.Fatal(err)
	}
	return artifactDir
}
