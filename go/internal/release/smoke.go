package release

import "purser-hook/internal/proc"

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var verifyDistributionRuntime = func(binary, root, name string) error {
	if err := verifyMCP(binary); err != nil {
		return err
	}
	if err := verifyEvents(binary, root); err != nil {
		return err
	}
	return verifyMissingBinary(root, name)
}

func DistributionSmoke(artifactDir string) error {
	archives, err := filepath.Glob(filepath.Join(artifactDir, "purser-*.zip"))
	if err != nil {
		return err
	}
	if len(archives) != 1 {
		return fmt.Errorf("expected one purser-X.Y.Z.zip in %s", artifactDir)
	}
	archive := archives[0]
	archiveDigest, sums, err := VerifyArchive(artifactDir, archive)
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(artifactDir, "release-manifest.json"))
	if err != nil {
		return err
	}
	var manifest Manifest
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	goos, arch, name, err := HostTarget()
	if err != nil {
		return err
	}
	binaryName := "bin/" + name
	if sums[binaryName] == "" || manifest.BinaryDigests[binaryName] != sums[binaryName] {
		return fmt.Errorf("host binary digest is not covered by release metadata: %s", binaryName)
	}
	root, err := os.MkdirTemp("", "purser-distribution-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	binary, err := SafeExtract(archive, root, name)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	if digest(content) != sums[binaryName] {
		return fmt.Errorf("installed host binary digest differs from package metadata")
	}
	if err = verifyHookRegistration(root); err != nil {
		return err
	}
	if err = verifyDistributionRuntime(binary, root, name); err != nil {
		return err
	}
	b, _ := os.ReadFile(filepath.Join(artifactDir, "SHA256SUMS"))
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.HasSuffix(line, "  "+filepath.Base(archive)) {
			lines[i] = strings.Repeat("0", 64) + line[64:]
			break
		}
	}
	badFile := filepath.Join(root, "bad-SHA256SUMS")
	if err = os.WriteFile(badFile, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return err
	}
	if _, _, err = verifyArchiveWithSums(archive, badFile); err == nil || !strings.Contains(err.Error(), "archive checksum mismatch") {
		return fmt.Errorf("bad checksum was accepted")
	}
	fmt.Printf("{\"os\":%q,\"arch\":%q,\"selected_binary\":%q,\"digest\":%q,\"checks\":[\"archive checksum\",\"execute bit\",\"MCP initialize/ping\",\"direct lifecycle binary event\",\"direct enforcement binary event\",\"bad checksum rejected\",\"unsafe archive members rejected\"]}\n", goos, arch, name, archiveDigest)
	return nil
}

func verifyHookRegistration(root string) error {
	var config struct {
		Modules []string `json:"modules"`
	}
	b, err := os.ReadFile(filepath.Join(root, "hooks/hooks.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	var registered strings.Builder
	for _, module := range config.Modules {
		path := filepath.Clean(filepath.Join(root, "hooks", filepath.FromSlash(module)))
		rel, e := filepath.Rel(filepath.Join(root, "hooks"), path)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("hook module does not resolve inside plugin: %s", module)
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return fmt.Errorf("hook module does not resolve inside plugin: %s", module)
		}
		registered.Write(data)
		registered.WriteByte('\n')
	}
	text := registered.String()
	if !strings.Contains(text, "classic.SessionStart") || !strings.Contains(text, "classic.UserPromptSubmit") || !strings.Contains(text, "$.plugin.root}/bin/") {
		return fmt.Errorf("configured lifecycle hook registration is incomplete")
	}
	match := regexp.MustCompile(`from ['"](\./mod/enforcement\.js)['"]`).FindStringSubmatch(text)
	if len(match) != 2 {
		return fmt.Errorf("configured hook module does not import enforcement")
	}
	module := strings.TrimSuffix(strings.TrimPrefix(match[1], "./"), ".js") + ".ts"
	enforcement, err := os.ReadFile(filepath.Join(root, "hooks", filepath.FromSlash(module)))
	if err != nil {
		return err
	}
	if !strings.Contains(string(enforcement), "classic.PreToolUse") || !strings.Contains(string(enforcement), "$.plugin.root}/bin/") {
		return fmt.Errorf("enforcement hook does not resolve the plugin bin path")
	}
	return nil
}
func verifyMissingBinary(root, selected string) error {
	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("Node.js is required for launcher checks")
	}
	isolated := filepath.Join(root, "missing-plugin", "bin")
	if err = os.MkdirAll(isolated, 0755); err != nil {
		return err
	}
	launcher, err := os.ReadFile(filepath.Join(root, "bin/purser-launcher.js"))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(isolated, "purser-launcher.js"), launcher, 0755); err != nil {
		return err
	}
	cmd := proc.Hide(exec.Command(node, filepath.Join(isolated, "purser-launcher.js"), "go-mcp"))
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil || !strings.Contains(stderr.String(), "Go binary missing") {
		return fmt.Errorf("missing binary did not produce the existing launcher error")
	}
	return nil
}
func verifyArchiveWithSums(archive, sumsPath string) (string, map[string]string, error) {
	s, e := ReadSums(sumsPath)
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
func verifyMCP(binary string) error {
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\"}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := proc.Hide(exec.CommandContext(ctx, binary, "mcp"))
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("MCP process exited: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	replies := map[int]map[string]any{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if id, ok := m["id"].(float64); ok {
			result, _ := m["result"].(map[string]any)
			replies[int(id)] = result
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	info, _ := replies[1]["serverInfo"].(map[string]any)
	if info["name"] != "codex-exec-mcp" {
		return fmt.Errorf("MCP initialize response was invalid")
	}
	if replies[2] == nil {
		return fmt.Errorf("MCP ping response was invalid")
	}
	return nil
}
func verifyEvents(binary, root string) error {
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	events := []struct {
		args []string
		data string
	}{{nil, `{"hook_event_name":"UserPromptSubmit","session_id":"distribution-smoke","cwd":"","prompt":"distribution smoke"}`}, {[]string{"enforce"}, fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":"distribution-smoke","cwd":%q,"tool_name":"Bash","tool_input":{"command":"true"}}`, root)}}
	for _, e := range events {
		cmd := proc.Hide(exec.Command(binary, e.args...))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PURSER_STATE_DIR="+state, "HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
		cmd.Stdin = strings.NewReader(e.data)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("hook event exited: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
