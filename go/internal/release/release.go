package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var Targets = [][2]string{{"windows", "amd64"}, {"windows", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
var modSources = []string{"hooks/register.ts", "hooks/mod/read.ts", "hooks/mod/lifecycle.ts", "hooks/mod/board.ts", "hooks/mod/usage.ts", "hooks/mod/enforcement.ts", "hooks/mod/autocompact.ts"}
var runtimeFiles = []string{"bin/purser-launcher.js", "plugin/mcp.json", "hooks/hooks.json", ".claude/hooks/orchestrator/config.json", ".claude/hooks/orchestrator/model_catalog_bootstrap.json"}
var commandFiles = []string{"commands/check.md", "commands/gear-down.md", "commands/gear-up.md", "commands/init.md", "commands/optimize.md", "commands/plan.md"}

type Manifest struct {
	PluginVersion string            `json:"plugin_version"`
	SourceCommit  string            `json:"source_commit"`
	GoToolchain   string            `json:"go_toolchain"`
	BuildTags     string            `json:"build_tags"`
	BuildFlags    []string          `json:"build_flags"`
	Targets       []Target          `json:"targets"`
	BinaryDigests map[string]string `json:"binary_digests"`
}
type Target struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Binary string `json:"binary"`
}

func binaryName(goos, arch string) string {
	suffix := ""
	if goos == "windows" {
		suffix = ".exe"
	}
	return "purser-hook-" + goos + "-" + arch + suffix
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func run(root string, env []string, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	c.Dir = root
	c.Env = env
	var out, errout bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errout
	err := c.Run()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errout.String()))
	}
	return out.Bytes(), nil
}
func git(root string, args ...string) ([]byte, error) { return run(root, os.Environ(), "git", args...) }

func MarketplacePath(root, value string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, value[2:])
		if value == filepath.Join(home, "") {
			value = home
		}
	}
	p, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	p, err = resolvePath(p)
	if err != nil {
		return "", err
	}
	r, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	r, err = resolvePath(r)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r, p)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("--marketplace-dir must be outside the repository")
	}
	return p, nil
}

func resolvePath(path string) (string, error) {
	var tail []string
	probe := path
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("cannot resolve path %s", path)
		}
		tail = append([]string{filepath.Base(probe)}, tail...)
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	for _, part := range tail {
		resolved = filepath.Join(resolved, part)
	}
	return filepath.Clean(resolved), nil
}

func stageCommit(root, commit, dest string) error {
	b, err := git(root, "archive", "--format=tar", commit)
	if err != nil {
		return fmt.Errorf("git archive failed: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	base, _ := filepath.Abs(dest)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := filepath.FromSlash(h.Name)
		if filepath.IsAbs(name) {
			return fmt.Errorf("unsafe path in git archive: %s", h.Name)
		}
		out := filepath.Join(base, name)
		rel, e := filepath.Rel(base, out)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in git archive: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeDir:
			if e = os.MkdirAll(out, os.FileMode(h.Mode)&0777); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			if e = os.MkdirAll(filepath.Dir(out), 0755); e != nil {
				return e
			}
			f, e := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0777)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, tr)
			ce := f.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		case tar.TypeSymlink:
			target := h.Linkname
			if filepath.IsAbs(target) {
				return fmt.Errorf("unsafe link in git archive: %s", h.Name)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(out), target))
			rel, e := filepath.Rel(base, resolved)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("unsafe link in git archive: %s", h.Name)
			}
			if e = os.MkdirAll(filepath.Dir(out), 0755); e != nil {
				return e
			}
			if e = os.Symlink(target, out); e != nil {
				return e
			}
		case tar.TypeLink:
			return fmt.Errorf("hard link in git archive is not supported: %s", h.Name)
		default:
			return fmt.Errorf("unsupported git archive member: %s", h.Name)
		}
	}
	return filterPython(dest)
}
func filterPython(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		if d.IsDir() && d.Name() == "__pycache__" {
			return os.RemoveAll(p)
		}
		if !d.IsDir() && (strings.EqualFold(filepath.Ext(d.Name()), ".py") || strings.EqualFold(filepath.Ext(d.Name()), ".pyc")) {
			return os.Remove(p)
		}
		return nil
	})
}
func grammarTags(root string) (string, error) {
	b, e := os.ReadFile(filepath.Join(root, "go/internal/enforce/code_map_grammar_tags.go"))
	if e != nil {
		return "", e
	}
	const marker = "const codeMapGrammarTags = \""
	s := string(b)
	i := strings.Index(s, marker)
	if i < 0 {
		return "", errors.New("Go grammar tags are missing or malformed")
	}
	s = s[i+len(marker):]
	j := strings.IndexByte(s, '"')
	if j < 0 {
		return "", errors.New("Go grammar tags are missing or malformed")
	}
	return strings.Join(strings.Fields(s[:j]), ","), nil
}
func buildHooks(root string) error {
	tags, e := grammarTags(root)
	if e != nil {
		return e
	}
	for _, t := range Targets {
		name := binaryName(t[0], t[1])
		out := filepath.Join(root, "bin", name)
		if e = os.MkdirAll(filepath.Dir(out), 0755); e != nil {
			return e
		}
		cmd := exec.Command("go", "build", "-tags", tags, "-trimpath", "-ldflags", "-s -w", "-o", out, "./cmd/purser-hook")
		cmd.Dir = filepath.Join(root, "go")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t[0], "GOARCH="+t[1])
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = &stderr
		if e = cmd.Run(); e != nil {
			return fmt.Errorf("Go build failed for %s/%s: %w: %s", t[0], t[1], e, strings.TrimSpace(stderr.String()))
		}
		if _, e = os.Stat(out); e != nil {
			return fmt.Errorf("Go build produced no binary for %s/%s", t[0], t[1])
		}
		if t[0] != "windows" {
			if e = os.Chmod(out, 0755); e != nil {
				return e
			}
		}
	}
	return nil
}
func VerifyTree(root string, binaries bool) error {
	required := append(append(append([]string{}, modSources...), runtimeFiles...), commandFiles...)
	if binaries {
		for _, t := range Targets {
			required = append(required, "bin/"+binaryName(t[0], t[1]))
		}
	}
	var missing []string
	for _, p := range required {
		if st, e := os.Stat(filepath.Join(root, filepath.FromSlash(p))); e != nil || !st.Mode().IsRegular() {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("staged export is missing required file(s): %s", strings.Join(missing, ", "))
	}
	var py []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if p == root {
			return nil
		}
		if d.Name() == "__pycache__" {
			py = append(py, filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator))))
		}
		if !d.IsDir() && (strings.EqualFold(filepath.Ext(d.Name()), ".py") || strings.EqualFold(filepath.Ext(d.Name()), ".pyc")) {
			rel, _ := filepath.Rel(root, p)
			py = append(py, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(py) > 0 {
		sort.Strings(py)
		return fmt.Errorf("staged export contains Python artifact(s): %s", strings.Join(py, ", "))
	}
	return nil
}

func ExportMarketplace(root, commit, marketplace string) error {
	if err := os.MkdirAll(marketplace, 0755); err != nil {
		return fmt.Errorf("marketplace export (prepare): %w", err)
	}
	stage, err := os.MkdirTemp(marketplace, "purser-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = stageCommit(root, commit, stage); err != nil {
		return fmt.Errorf("marketplace export (extract archive): %w", err)
	}
	if err = VerifyTree(stage, false); err != nil {
		return err
	}
	if err = buildHooks(stage); err != nil {
		return err
	}
	if err = VerifyTree(stage, true); err != nil {
		return err
	}
	manifest := map[string]any{"name": "purser-local", "description": "Local marketplace for the Purser plugin.", "owner": map[string]string{"name": "Purser"}, "plugins": []map[string]string{{"name": "purser", "source": "./purser"}}}
	manifestStage := filepath.Join(marketplace, fmt.Sprintf(".claude-plugin-stage-%d", os.Getpid()))
	backup := filepath.Join(marketplace, fmt.Sprintf(".purser-previous-%d", os.Getpid()))
	backupManifest := filepath.Join(marketplace, fmt.Sprintf(".claude-plugin-previous-%d", os.Getpid()))
	live := filepath.Join(marketplace, "purser")
	liveManifest := filepath.Join(marketplace, ".claude-plugin")
	for _, p := range []string{manifestStage, backup, backupManifest} {
		if _, e := os.Lstat(p); e == nil {
			return errors.New("stale marketplace export staging path exists")
		}
	}
	if err = os.Mkdir(manifestStage, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(manifestStage)
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if err = os.WriteFile(filepath.Join(manifestStage, "marketplace.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	hadTree := exists(live)
	hadManifest := exists(liveManifest)
	treeMoved := false
	manifestMoved := false
	rollback := func() {
		if manifestMoved {
			_ = os.RemoveAll(liveManifest)
		}
		if hadManifest && exists(backupManifest) {
			_ = os.Rename(backupManifest, liveManifest)
		}
		if treeMoved {
			_ = os.RemoveAll(live)
		}
		if hadTree && exists(backup) {
			_ = os.Rename(backup, live)
		}
	}
	if hadTree {
		if err = os.Rename(live, backup); err != nil {
			return fmt.Errorf("marketplace export (backup previous tree): %w", err)
		}
	}
	if err = os.Rename(stage, live); err != nil {
		rollback()
		return fmt.Errorf("marketplace export (publish tree): %w", err)
	}
	treeMoved = true
	if hadManifest {
		if err = os.Rename(liveManifest, backupManifest); err != nil {
			rollback()
			return fmt.Errorf("marketplace export (backup previous manifest): %w", err)
		}
	}
	if err = os.Rename(manifestStage, liveManifest); err != nil {
		rollback()
		return fmt.Errorf("marketplace export (publish manifest): %w", err)
	}
	manifestMoved = true
	_ = os.RemoveAll(backup)
	_ = os.RemoveAll(backupManifest)
	return nil
}
func exists(p string) bool { _, e := os.Lstat(p); return e == nil }

func Package(root, commit, output string) (string, error) {
	if err := os.MkdirAll(output, 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(output, "purser-release-stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = stageCommit(root, commit, stage); err != nil {
		return "", err
	}
	if err = VerifyTree(stage, false); err != nil {
		return "", err
	}
	if err = buildHooks(stage); err != nil {
		return "", err
	}
	if err = VerifyTree(stage, true); err != nil {
		return "", err
	}
	var plugin struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(stage, ".claude-plugin/plugin.json"))
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal(b, &plugin); err != nil {
		return "", err
	}
	parts := strings.Split(plugin.Version, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid semantic version: %s", plugin.Version)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("invalid semantic version: %s", plugin.Version)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid semantic version: %s", plugin.Version)
			}
		}
	}
	tool, err := run(filepath.Join(stage, "go"), os.Environ(), "go", "version")
	if err != nil {
		return "", fmt.Errorf("cannot determine Go toolchain version: %w", err)
	}
	tags, err := grammarTags(stage)
	if err != nil {
		return "", err
	}
	binDigests := map[string]string{}
	var targets []Target
	for _, t := range Targets {
		n := "bin/" + binaryName(t[0], t[1])
		content, e := os.ReadFile(filepath.Join(stage, filepath.FromSlash(n)))
		if e != nil {
			return "", fmt.Errorf("required binary missing: %s", n)
		}
		binDigests[n] = digest(content)
		targets = append(targets, Target{t[0], t[1], n})
	}
	archivePath := filepath.Join(output, "purser-"+plugin.Version+".zip")
	if err = writeArchive(stage, archivePath, binDigests); err != nil {
		return "", err
	}
	if err = verifyArchivedBinaries(archivePath, binDigests); err != nil {
		return "", err
	}
	manifest := Manifest{plugin.Version, commit, strings.TrimSpace(string(tool)), tags, []string{"-trimpath", "-ldflags", "-s -w"}, targets, binDigests}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err = os.WriteFile(filepath.Join(output, "release-manifest.json"), append(mb, '\n'), 0644); err != nil {
		return "", err
	}
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		return "", err
	}
	lines := []string{digest(archiveData) + "  " + filepath.Base(archivePath)}
	for _, t := range targets {
		lines = append(lines, binDigests[t.Binary]+"  "+t.Binary)
	}
	if err = os.WriteFile(filepath.Join(output, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return "", err
	}
	return archivePath, nil
}

func writeArchive(root, archivePath string, binaryDigests map[string]string) error {
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		name := filepath.ToSlash(rel)
		content, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		mode := os.FileMode(0644)
		if st, e := os.Stat(p); e == nil {
			mode = st.Mode().Perm()
		}
		if _, ok := binaryDigests[name]; ok && !strings.Contains(name, "windows") {
			mode = 0755
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, e := zw.CreateHeader(h)
		if e != nil {
			return e
		}
		_, e = w.Write(content)
		return e
	})
	if err == nil {
		err = zw.Close()
	} else {
		_ = zw.Close()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}
func verifyArchivedBinaries(archivePath string, binaryDigests map[string]string) error {
	verify, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer verify.Close()
	byName := make(map[string]*zip.File, len(verify.File))
	for _, f := range verify.File {
		byName[f.Name] = f
	}
	for name, want := range binaryDigests {
		f := byName[name]
		if f == nil {
			return fmt.Errorf("release archive missing binary: %s", name)
		}
		r, e := f.Open()
		if e != nil {
			return e
		}
		content, e := io.ReadAll(r)
		_ = r.Close()
		if e != nil {
			return e
		}
		if digest(content) != want {
			return fmt.Errorf("release archive binary digest mismatch: %s", name)
		}
		if !strings.Contains(name, "windows") && f.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("release archive lost Unix execute bit: %s", name)
		}
	}
	return nil
}

func HostTarget() (string, string, string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	if osName != "windows" && osName != "linux" && osName != "darwin" {
		return "", "", "", fmt.Errorf("unsupported host: %s/%s", osName, arch)
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", "", fmt.Errorf("unsupported host: %s/%s", osName, arch)
	}
	return osName, arch, binaryName(osName, arch), nil
}
func ReadSums(path string) (map[string]string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, fmt.Errorf("invalid SHA256SUMS entry: %q", line)
		}
		d, n := strings.ToLower(line[:64]), line[66:]
		if _, e := hex.DecodeString(d); e != nil || len(d) != 64 || n == "" {
			return nil, fmt.Errorf("invalid SHA256SUMS entry: %q", line)
		}
		if _, ok := out[n]; ok {
			return nil, fmt.Errorf("invalid SHA256SUMS entry: %q", line)
		}
		out[n] = d
	}
	return out, nil
}
func VerifyArchive(artifactDir, archive string) (string, map[string]string, error) {
	s, e := ReadSums(filepath.Join(artifactDir, "SHA256SUMS"))
	if e != nil {
		return "", nil, e
	}
	b, e := os.ReadFile(archive)
	if e != nil {
		return "", nil, e
	}
	d := digest(b)
	if s[filepath.Base(archive)] != d {
		return "", nil, fmt.Errorf("archive checksum mismatch: %s", filepath.Base(archive))
	}
	return d, s, nil
}

func SafeExtract(archive, dest, selected string) (string, error) {
	r, e := zip.OpenReader(archive)
	if e != nil {
		return "", e
	}
	defer r.Close()
	base, e := filepath.Abs(dest)
	if e != nil {
		return "", e
	}
	var chosen *zip.File
	for _, f := range r.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasPrefix(name, "/") || filepath.VolumeName(name) != "" || strings.HasPrefix(name, "//") || len(name) >= 2 && name[1] == ':' {
			return "", fmt.Errorf("unsafe archive member: %s", f.Name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return "", fmt.Errorf("unsafe archive member: %s", f.Name)
			}
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("archive symlink is not supported: %s", f.Name)
		}
		out := filepath.Join(base, filepath.FromSlash(name))
		rel, e := filepath.Rel(base, out)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe archive member: %s", f.Name)
		}
		if f.Name == "bin/"+selected {
			chosen = f
		}
	}
	if chosen == nil {
		return "", fmt.Errorf("host binary missing from archive: bin/%s", selected)
	}
	if chosen.Mode().Perm()&0111 == 0 {
		return "", errors.New("host binary execute bit missing from archive")
	}
	if e = os.MkdirAll(base, 0755); e != nil {
		return "", e
	}
	for _, f := range r.File {
		out := filepath.Join(base, filepath.FromSlash(strings.ReplaceAll(f.Name, "\\", "/")))
		if f.FileInfo().IsDir() {
			if e = os.MkdirAll(out, 0755); e != nil {
				return "", e
			}
			continue
		}
		if e = os.MkdirAll(filepath.Dir(out), 0755); e != nil {
			return "", e
		}
		src, e := f.Open()
		if e != nil {
			return "", e
		}
		data, e := io.ReadAll(src)
		_ = src.Close()
		if e != nil {
			return "", e
		}
		if e = os.WriteFile(out, data, f.Mode().Perm()); e != nil {
			return "", e
		}
	}
	return filepath.Join(base, "bin", selected), nil
}

func Catalog(url, checksum string) ([]byte, error) {
	if len(checksum) != 64 {
		return nil, errors.New("invalid SHA256 checksum")
	}
	if _, e := hex.DecodeString(checksum); e != nil {
		return nil, errors.New("invalid SHA256 checksum")
	}
	v := map[string]any{"name": "purser", "source": map[string]string{"source": "archive", "url": url, "sha256": strings.ToLower(checksum)}}
	b, e := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), e
}
