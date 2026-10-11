package release

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyTreeRequiresRuntimeAndRejectsPythonArtifacts(t *testing.T) {
	root := t.TempDir()
	for _, p := range append(append(append([]string{}, modSources...), runtimeFiles...), commandFiles...) {
		path := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyTree(root, false); err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, "injected.py")
	if err := os.WriteFile(py, []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTree(root, false); err == nil || !strings.Contains(err.Error(), "injected.py") {
		t.Fatalf("Python artifact accepted: %v", err)
	}
	_ = os.Remove(py)
	_ = os.Remove(filepath.Join(root, "bin/purser-launcher.js"))
	if err := VerifyTree(root, false); err == nil || !strings.Contains(err.Error(), "bin/purser-launcher.js") {
		t.Fatalf("missing runtime asset accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin/purser-launcher.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range modSources {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTree(root, false); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("missing mod source %s accepted: %v", name, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range Targets {
		name := "bin/" + binaryName(target[0], target[1])
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyTree(root, true); err != nil {
		t.Fatalf("complete staged tree rejected: %v", err)
	}
	missingBinary := "bin/" + binaryName(Targets[0][0], Targets[0][1])
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(missingBinary))); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTree(root, true); err == nil || !strings.Contains(err.Error(), missingBinary) {
		t.Fatalf("missing target binary accepted: %v", err)
	}
}

func TestSafeExtractRejectsUnsafeMembersAndChecksExecutable(t *testing.T) {
	for _, name := range []string{`nested\..\..\escape.txt`, `C:\escape.txt`, `/escape.txt`, `../escape.txt`, "symlink"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "bad.zip")
			f, e := os.Create(archive)
			if e != nil {
				t.Fatal(e)
			}
			z := zip.NewWriter(f)
			h := &zip.FileHeader{Name: name}
			if name == "symlink" {
				h.SetMode(os.ModeSymlink | 0777)
			}
			_, e = z.CreateHeader(h)
			if e != nil {
				t.Fatal(e)
			}
			_ = z.Close()
			_ = f.Close()
			if _, e = SafeExtract(archive, filepath.Join(root, "out"), "purser-hook-linux-amd64"); e == nil {
				t.Fatal("unsafe member accepted")
			}
		})
	}
	root := t.TempDir()
	archive := filepath.Join(root, "noexec.zip")
	f, _ := os.Create(archive)
	z := zip.NewWriter(f)
	h := &zip.FileHeader{Name: "bin/purser-hook-linux-amd64"}
	h.SetMode(0644)
	w, _ := z.CreateHeader(h)
	_, _ = w.Write([]byte("binary"))
	_ = z.Close()
	_ = f.Close()
	if _, e := SafeExtract(archive, filepath.Join(root, "out"), "purser-hook-linux-amd64"); e == nil || !strings.Contains(e.Error(), "execute bit") {
		t.Fatalf("non-executable host binary accepted: %v", e)
	}
}

func TestFailedExportPreservesMarketplaceTree(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, args...)...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v: %s", args, e, b)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("committed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "sentinel")
	runGit("-c", "user.name=Release Test", "-c", "user.email=release@example.test", "commit", "-qm", "fixture")
	market := filepath.Join(t.TempDir(), "marketplace")
	live := filepath.Join(market, "purser")
	if err := os.MkdirAll(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "sentinel"), []byte("previous\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := ExportMarketplace(root, "HEAD", market)
	if err == nil || !strings.Contains(err.Error(), "staged export is missing") {
		t.Fatalf("invalid staged tree unexpectedly exported: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(live, "sentinel"))
	if err != nil || string(b) != "previous\n" {
		t.Fatalf("previous tree was not preserved: %q %v", b, err)
	}
	entries, err := os.ReadDir(market)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "purser-export-") {
			t.Fatalf("staging directory leaked: %s", entry.Name())
		}
	}
}

func TestExportMarketplaceUsesCommittedTreeAndLocalManifest(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range append(append(append([]string{}, modSources...), runtimeFiles...), commandFiles...) {
		write(name, "asset\n")
	}
	write(".claude-plugin/plugin.json", "{\"name\":\"purser\",\"version\":\"0.1.12\"}\n")
	write("go/internal/enforce/code_map_grammar_tags.go", "package enforce\nconst codeMapGrammarTags = \"release_test\"\n")
	write("base.txt", "committed\n")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "core.autocrlf", "false")
	runGit("add", ".")
	runGit("-c", "user.name=Release Test", "-c", "user.email=release@example.test", "commit", "-qm", "fixture")
	write("base.txt", "working copy edit\n")
	write("ignored.txt", "must not ship\n")

	fakeGoSource := filepath.Join(t.TempDir(), "fake-go.go")
	fakeGo := filepath.Join(filepath.Dir(fakeGoSource), "go")
	if runtime.GOOS == "windows" {
		fakeGo += ".exe"
	}
	program := "package main\nimport \"os\"\nfunc main() { args := os.Args[1:]; for i, arg := range args { if arg == \"-o\" && i+1 < len(args) { if err := os.WriteFile(args[i+1], []byte(\"fake binary\"), 0700); err != nil { os.Exit(2) }; return } }; if len(args) > 0 && args[0] == \"version\" { println(\"go version fake\") ; return }; os.Exit(2) }\n"
	if err := os.WriteFile(fakeGoSource, []byte(program), 0644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", fakeGo, fakeGoSource)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake go: %v: %s", err, out)
	}
	binDir := filepath.Dir(fakeGo)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	market := filepath.Join(t.TempDir(), "marketplace")
	if err := ExportMarketplace(root, "HEAD", market); err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(market, "purser")
	if b, err := os.ReadFile(filepath.Join(exported, "base.txt")); err != nil || string(b) != "committed\n" {
		t.Fatalf("export used working tree instead of committed tree: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(exported, "ignored.txt")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted file was exported: %v", err)
	}
	for _, target := range Targets {
		if _, err := os.Stat(filepath.Join(exported, "bin", binaryName(target[0], target[1]))); err != nil {
			t.Errorf("export missing hook binary for %s/%s: %v", target[0], target[1], err)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(market, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "purser-local" || manifest["description"] != "Local marketplace for the Purser plugin." {
		t.Fatalf("unexpected marketplace manifest: %s", manifestBytes)
	}
	plugins, ok := manifest["plugins"].([]any)
	if !ok || len(plugins) != 1 || plugins[0].(map[string]any)["source"] != "./purser" {
		t.Fatalf("unexpected marketplace plugin list: %s", manifestBytes)
	}
}

func TestReadSumsAndVerifyArchive(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "purser-1.2.3.zip")
	data := []byte("archive")
	if err := os.WriteFile(archive, data, 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(sum+"  purser-1.2.3.zip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, _, err := VerifyArchive(root, archive)
	if err != nil || got != sum {
		t.Fatalf("valid archive verification failed: %s %v", got, err)
	}
	if err = os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  purser-1.2.3.zip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = VerifyArchive(root, archive); err == nil || !strings.Contains(err.Error(), "archive checksum mismatch") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
}

func TestArchiveContainsSixTargetsWithDigestsAndUnixModes(t *testing.T) {
	root := t.TempDir()
	digests := map[string]string{}
	for _, target := range Targets {
		name := "bin/" + binaryName(target[0], target[1])
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		data := []byte(target[0] + "/" + target[1])
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		digests[name] = digest(data)
	}
	for _, name := range append(append([]string{}, runtimeFiles...), commandFiles...) {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "purser-test.zip")
	if err := writeArchive(root, archive, digests); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchivedBinaries(archive, digests); err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	names := map[string]*zip.File{}
	for _, f := range r.File {
		names[f.Name] = f
	}
	for _, name := range append(append([]string{}, runtimeFiles...), commandFiles...) {
		if names[name] == nil {
			t.Errorf("archive missing %s", name)
		}
	}
	for name, want := range digests {
		f := names[name]
		if f == nil {
			t.Errorf("archive missing %s", name)
			continue
		}
		src, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(src)
		_ = src.Close()
		if e != nil || digest(data) != want {
			t.Errorf("archive digest mismatch for %s: %v", name, e)
		}
		if !strings.Contains(name, "windows") && f.Mode().Perm()&0111 == 0 {
			t.Errorf("Unix execute bit missing for %s", name)
		}
	}
	if err := verifyArchivedBinaries(archive, map[string]string{"bin/purser-hook-linux-amd64": strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt digest expectation accepted: %v", err)
	}
}

func TestCatalogContract(t *testing.T) {
	sum := strings.Repeat("a", 64)
	b, err := Catalog("https://example.test/purser.zip", sum)
	if err != nil {
		t.Fatal(err)
	}
	want := "\"source\": \"archive\""
	if !strings.Contains(string(b), want) || !strings.Contains(string(b), sum) || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("unexpected catalog: %s", b)
	}
	if _, err = Catalog("url", "bad"); err == nil {
		t.Fatal("invalid checksum accepted")
	}
}

func TestMarketplacePathMustResolveOutsideRepository(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := MarketplacePath(root, filepath.Join(root, "marketplace")); err == nil {
		t.Fatal("repository child accepted as marketplace")
	}
	if _, err := MarketplacePath(root, filepath.Join(parent, "marketplace")); err != nil {
		t.Fatalf("external marketplace rejected: %v", err)
	}
}
