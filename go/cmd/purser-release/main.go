package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/release"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "purser-release:", err)
		os.Exit(1)
	}
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

func run(args []string) error {
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "package" {
		fs := flag.NewFlagSet("package", flag.ContinueOnError)
		commit := fs.String("commit", "HEAD", "committed source tree")
		out := fs.String("output-dir", "dist", "artifact directory")
		if err = fs.Parse(args[1:]); err != nil {
			return err
		}
		path, e := release.Package(root, *commit, *out)
		if e != nil {
			return e
		}
		fmt.Println(path)
		return nil
	}
	if len(args) > 0 && args[0] == "smoke" {
		fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
		dir := fs.String("artifact-dir", "dist", "artifact directory")
		if err = fs.Parse(args[1:]); err != nil {
			return err
		}
		return release.DistributionSmoke(*dir)
	}
	if len(args) > 0 && args[0] == "catalog" {
		fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
		url := fs.String("url", "", "release archive URL")
		sum := fs.String("sha256", "", "archive checksum")
		out := fs.String("output", "catalog-entry.json", "catalog output path")
		if err = fs.Parse(args[1:]); err != nil {
			return err
		}
		if *url == "" {
			return fmt.Errorf("catalog requires --url")
		}
		b, e := release.Catalog(*url, *sum)
		if e != nil {
			return e
		}
		return os.WriteFile(*out, b, 0644)
	}
	args, err = expandPaths(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("purser-release", flag.ContinueOnError)
	export := fs.Bool("export-only", false, "export HEAD to the local marketplace")
	dry := fs.Bool("dry-run", false, "print the export plan without changing files")
	market := fs.String("marketplace-dir", defaultMarketplace(), "local marketplace directory")
	claude := fs.String("claude-bin", "claude", "Claude CLI executable")
	entry := fs.String("entry", "", "CHANGELOG bullet text")
	title := fs.String("title", "", "release commit title")
	version := fs.String("version", "", "release version (default: current patch + 1)")
	message := fs.String("message", "", "change commit subject (required with --paths)")
	var paths stringList
	fs.Var(&paths, "path-item", "changed file included in the change commit")
	if err = fs.Parse(args); err != nil {
		return err
	}
	target, err := release.MarketplacePath(root, *market)
	if err != nil {
		return err
	}
	b, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(b))
	if *export {
		if *version != "" || *message != "" || len(paths) > 0 {
			return fmt.Errorf("--export-only cannot be combined with --paths, --message, or --version")
		}
		if *dry {
			fmt.Printf("dry-run: export %s to %s\n", commit[:7], target)
			return nil
		}
		fmt.Println("export target", target)
		if err = release.ExportMarketplace(root, commit, target); err != nil {
			return err
		}
		fmt.Println("exported", commit[:7])
	} else {
		if strings.TrimSpace(*entry) == "" || strings.TrimSpace(*title) == "" {
			return fmt.Errorf("--entry and --title must not be empty")
		}
		if len(paths) > 0 && strings.TrimSpace(*message) == "" {
			return fmt.Errorf("--message is required with --paths")
		}
		if len(paths) == 0 && *message != "" {
			return fmt.Errorf("--message requires --paths")
		}
		if strings.ContainsAny(*message+*title, "\r\n") {
			return fmt.Errorf("--message and --title must be single-line commit subjects")
		}
		commit, err = releaseVersion(root, *version, strings.TrimSpace(*entry), strings.TrimSpace(*title), *dry, target, paths, *message)
		if err != nil {
			return err
		}
		if *dry {
			return nil
		}
		fmt.Println("export target", target)
		if err = release.ExportMarketplace(root, commit, target); err != nil {
			return err
		}
		fmt.Println("exported", commit[:7])
	}
	for i, args := range [][]string{{"plugin", "marketplace", "update", "purser-local"}, {"plugin", "update", "purser@purser-local"}} {
		cmd := exec.Command(*claude, args...)
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
		}
		if i == 0 {
			fmt.Println("marketplace update ok")
		} else {
			fmt.Println("plugin update ok")
		}
	}
	return nil
}

func expandPaths(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] != "--paths" {
			out = append(out, args[i])
			continue
		}
		i++
		start := len(out)
		for i < len(args) && !strings.HasPrefix(args[i], "-") {
			out = append(out, "--path-item="+args[i])
			i++
		}
		i--
		if len(out) == start {
			return nil, fmt.Errorf("--paths requires at least one path")
		}
	}
	return out, nil
}

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
var headingRE = regexp.MustCompile(`(?m)^## (\d+\.\d+\.\d+) — (\d{4}-\d{2}-\d{2})\s*$`)
var releaseSubjectRE = regexp.MustCompile(`^purser: release (\d+\.\d+\.\d+) \((.*)\)$`)

func releaseVersion(root, requested, entry, title string, dry bool, target string, paths []string, message string) (string, error) {
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(head))
	validated := make([]string, 0, len(paths))
	pathSet := map[string]bool{}
	for _, path := range paths {
		normalized := strings.ReplaceAll(path, "\\", "/")
		for strings.HasPrefix(normalized, "./") {
			normalized = strings.TrimPrefix(normalized, "./")
		}
		unsafe := normalized == "" || strings.HasPrefix(normalized, "/") || len(normalized) >= 2 && normalized[1] == ':' || normalized == "CHANGELOG.md" || normalized == ".claude-plugin/plugin.json"
		for _, part := range strings.Split(normalized, "/") {
			if part == ".." {
				unsafe = true
			}
		}
		if unsafe {
			return "", fmt.Errorf("unsafe change path: %s", path)
		}
		if pathSet[normalized] {
			return "", fmt.Errorf("--paths contains duplicates")
		}
		pathSet[normalized] = true
		validated = append(validated, normalized)
	}
	status, err := exec.Command("git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return "", err
	}
	changed := map[string]bool{}
	for _, record := range bytes.Split(status, []byte{0}) {
		if len(record) < 4 {
			continue
		}
		name := strings.ReplaceAll(string(record[3:]), "\\", "/")
		changed[name] = true
		if record[0] != ' ' && record[0] != '?' && !pathSet[name] {
			return "", fmt.Errorf("staged changes outside --paths: %s", name)
		}
	}
	if len(validated) > 0 {
		var missing []string
		for _, path := range validated {
			if !changed[path] {
				missing = append(missing, path)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return "", fmt.Errorf("listed path has no change: %s", strings.Join(missing, ", "))
		}
	}
	pluginPath := filepath.Join(root, ".claude-plugin/plugin.json")
	raw, err := os.ReadFile(pluginPath)
	if err != nil {
		return "", err
	}
	var plugin map[string]any
	if err = json.Unmarshal(raw, &plugin); err != nil {
		return "", err
	}
	current, _ := plugin["version"].(string)
	if !versionRE.MatchString(current) {
		return "", fmt.Errorf("invalid semantic version: %s", current)
	}
	headInfo, err := exec.Command("git", "-C", root, "show", "-s", "--format=%H%x00%P%x00%s", "HEAD").Output()
	if err != nil {
		return "", err
	}
	headFields := strings.SplitN(string(headInfo), "\x00", 3)
	if len(headFields) != 3 {
		return "", fmt.Errorf("cannot inspect HEAD")
	}
	headCommit := strings.TrimSpace(headFields[0])
	parent := ""
	if strings.TrimSpace(headFields[1]) != "" {
		parent = strings.Fields(headFields[1])[0]
	}
	subject := strings.TrimSpace(headFields[2])
	releaseMatch := releaseSubjectRE.FindStringSubmatch(subject)
	version := requested
	if version == "" {
		resumeCandidate := len(releaseMatch) == 3 && releaseMatch[1] == current && releaseMatch[2] == title
		for _, path := range validated {
			if changed[path] {
				resumeCandidate = false
			}
		}
		if resumeCandidate {
			version = current
		} else {
			p := strings.Split(current, ".")
			n, _ := strconv.Atoi(p[2])
			version = fmt.Sprintf("%s.%s.%d", p[0], p[1], n+1)
		}
	}
	if !versionRE.MatchString(version) {
		return "", fmt.Errorf("invalid semantic version: %s", version)
	}
	changelogPath := filepath.Join(root, "CHANGELOG.md")
	changelogBytes, err := os.ReadFile(changelogPath)
	if err != nil {
		return "", err
	}
	changelog := string(changelogBytes)
	newline := "\n"
	if strings.Contains(changelog, "\r\n") {
		newline = "\r\n"
	}
	matches := headingRE.FindAllStringSubmatchIndex(changelog, -1)
	insert := len(changelog)
	headerStart, headerEnd := -1, -1
	for _, m := range matches {
		if changelog[m[2]:m[3]] == version {
			headerStart, headerEnd = m[0], m[1]
		}
		if insert == len(changelog) {
			insert = m[0]
		}
	}
	if headerStart >= 0 {
		sectionEnd := len(changelog)
		for _, m := range matches {
			if m[0] > headerStart {
				sectionEnd = m[0]
				break
			}
		}
		section := changelog[headerEnd:sectionEnd]
		resumable := len(releaseMatch) == 3 && releaseMatch[1] == version && releaseMatch[2] == title && current == version && parent != ""
		if resumable {
			shortParent, err := exec.Command("git", "-C", root, "rev-parse", "--short=7", parent).Output()
			if err != nil {
				return "", err
			}
			resumable = strings.Contains(section, "(`"+strings.TrimSpace(string(shortParent))+"`).")
		}
		if !resumable {
			return "", fmt.Errorf("CHANGELOG header %s already exists for a different release commit", version)
		}
		if dry {
			fmt.Printf("dry-run: resume release %s at %s\n", version, headCommit[:7])
			fmt.Printf("dry-run: export %s to %s\n", headCommit[:7], target)
			return "", nil
		}
		return headCommit, nil
	}
	changeHash := commit[:7]
	if len(validated) > 0 {
		changeHash = "<new change commit>"
	}
	if dry {
		if len(validated) > 0 {
			fmt.Printf("dry-run: change commit %s\n", message)
		} else {
			fmt.Printf("dry-run: reference HEAD %s\n", changeHash)
		}
		fmt.Printf("dry-run: changelog ## %s — %s / %s\n", version, time.Now().Format("2006-01-02"), entry)
		fmt.Printf("dry-run: version %s\n", version)
		fmt.Printf("dry-run: release commit purser: release %s (%s)\n", version, title)
		fmt.Printf("dry-run: export <release commit> to %s\n", target)
		fmt.Println("dry-run: claude plugin marketplace update purser-local")
		fmt.Println("dry-run: claude plugin update purser@purser-local")
		return "", nil
	}
	if len(validated) > 0 {
		addArgs := append([]string{"-C", root, "--literal-pathspecs", "add", "-f", "--"}, validated...)
		if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("change commit git add failed: %s", strings.TrimSpace(string(out)))
		}
		commitArgs := append([]string{"-C", root, "--literal-pathspecs", "commit", "--only", "-m", message, "--"}, validated...)
		if out, err := exec.Command("git", commitArgs...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("change commit failed: %s", strings.TrimSpace(string(out)))
		}
		b, err := exec.Command("git", "-C", root, "rev-parse", "--short=7", "HEAD").Output()
		if err != nil {
			return "", err
		}
		changeHash = strings.TrimSpace(string(b))
	}
	cleanEntry := strings.TrimRight(entry, ".")
	heading := fmt.Sprintf("## %s — %s", version, time.Now().Format("2006-01-02"))
	bullet := fmt.Sprintf("%s (`%s`).", cleanEntry, changeHash)
	insertion := heading + newline + newline + bullet + newline + newline
	if insert < len(changelog) {
		changelog = changelog[:insert] + insertion + changelog[insert:]
	} else {
		changelog = strings.TrimRight(changelog, "\r\n") + newline + newline + insertion
	}
	if err = os.WriteFile(changelogPath, []byte(changelog), 0644); err != nil {
		return "", err
	}
	plugin["version"] = version
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(plugin); err != nil {
		return "", err
	}
	pluginText := buf.String()
	if strings.Contains(string(raw), "\r\n") {
		pluginText = strings.ReplaceAll(pluginText, "\n", "\r\n")
	}
	if err = os.WriteFile(pluginPath, []byte(pluginText), 0644); err != nil {
		return "", err
	}
	if _, err = exec.Command("git", "-C", root, "add", "--", "CHANGELOG.md", ".claude-plugin/plugin.json").CombinedOutput(); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("purser: release %s (%s)", version, title)
	cmd := exec.Command("git", "-C", root, "commit", "-m", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("release commit failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	b, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
func repositoryRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	p := cwd
	for {
		if _, e := os.Stat(filepath.Join(p, "go", "go.mod")); e == nil {
			if _, e = os.Stat(filepath.Join(p, ".git")); e == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return "", fmt.Errorf("cannot locate repository root from %s", cwd)
}
func defaultMarketplace() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("~", ".claude", "purser", "marketplace")
	}
	return filepath.Join(home, ".claude", "purser", "marketplace")
}
