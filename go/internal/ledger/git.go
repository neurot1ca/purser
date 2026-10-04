package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const indexPathspecByteBudget = 8000

type inspectionError struct {
	Code    string
	Message string
}

type InspectionError = inspectionError

func (e *inspectionError) Error() string { return e.Code + ": " + e.Message }

func runGit(root string, args []string, timeout time.Duration, operation string, input []byte, env map[string]string) ([]byte, error) {
	commandPath := ""
	if env != nil {
		if pathValue, ok := env["PATH"]; ok {
			resolved, found := findGit(pathValue, env["PATHEXT"])
			if !found {
				return nil, &inspectionError{"GIT_UNAVAILABLE", "Git could not run while " + operation + "."}
			}
			args = append([]string(nil), args...)
			commandPath = resolved
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	gitPath := "git"
	if env != nil && env["PATH"] != "" {
		gitPath = commandPath
	}
	command := exec.CommandContext(ctx, gitPath, args...)
	command.Dir = root
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	command.SysProcAttr = hiddenProcessAttributes()
	if env != nil {
		command.Env = os.Environ()
		for key, value := range env {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return nil, &inspectionError{"GIT_TIMEOUT", "Git timed out while " + operation + "."}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, &inspectionError{"GIT_FAILED", fmt.Sprintf("Git failed with exit code %d while %s.", exitErr.ExitCode(), operation)}
		}
		return nil, &inspectionError{"GIT_UNAVAILABLE", "Git could not run while " + operation + "."}
	}
	return []byte(stdout.String()), nil
}

// GitTopLevel resolves a directory through Git's work-tree semantics.
func GitTopLevel(directory string) (string, error) {
	raw, err := runGit(directory, []string{"rev-parse", "--show-toplevel"}, 10*time.Second, "resolving Git top-level", nil, nil)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(raw))
	if root == "" {
		return "", &inspectionError{"GIT_FAILED", "Git returned an empty top-level."}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", &inspectionError{"GIT_OUTPUT_INVALID", "Git returned an invalid top-level."}
	}
	return filepath.Abs(resolved)
}

func findGit(pathValue, pathExt string) (string, bool) {
	names := []string{"git"}
	if runtime.GOOS == "windows" {
		if pathExt == "" {
			pathExt = ".COM;.EXE;.BAT;.CMD"
		}
		for _, extension := range strings.Split(pathExt, ";") {
			names = append(names, "git"+strings.ToLower(extension))
		}
	}
	for _, directory := range strings.Split(pathValue, string(os.PathListSeparator)) {
		for _, name := range names {
			candidate := filepath.Join(directory, name)
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
				return candidate, true
			}
		}
	}
	return "", false
}

func normalizeScope(scope []string) ([]string, error) {
	if len(scope) == 0 {
		return nil, &inspectionError{"INVALID_SCOPE", "At least one scoped path is required."}
	}
	drive := regexp.MustCompile(`^[A-Za-z]:`)
	unique := make(map[string]bool)
	for _, value := range scope {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, &inspectionError{"INVALID_SCOPE", "Scope paths must be non-empty strings."}
		}
		value = strings.ReplaceAll(value, `\`, "/")
		if strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") || strings.Contains("/"+value+"/", "/../") || drive.MatchString(value) {
			return nil, &inspectionError{"INVALID_SCOPE", "Scope paths must be literal repository-relative paths."}
		}
		value = path.Clean(value)
		if value == "." || value == "" {
			return nil, &inspectionError{"UNBOUNDED_SCOPE", "A whole-repository manifest scope is forbidden."}
		}
		unique[value] = true
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func literalPathspecs(paths []string) []string {
	result := make([]string, len(paths))
	for i, value := range paths {
		result[i] = ":(literal)" + value
	}
	return result
}

func parseStatus(raw []byte) ([]map[string]string, error) {
	records := bytes.Split(raw, []byte{0})
	entries := make([]map[string]string, 0)
	for i := 0; i < len(records) && len(records[i]) > 0; i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' || record[0] > 127 || record[1] > 127 {
			return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git status output was malformed."}
		}
		entry := map[string]string{"state": string(record[:2]), "path": decodeGitPath(record[3:])}
		if strings.ContainsAny(entry["state"], "RC") {
			i++
			if i >= len(records) || len(records[i]) == 0 {
				return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git rename output was malformed."}
			}
			entry["original_path"] = decodeGitPath(records[i])
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i]["path"] == entries[j]["path"] {
			return entries[i]["state"] < entries[j]["state"]
		}
		return entries[i]["path"] < entries[j]["path"]
	})
	return entries, nil
}

func statusEntries(root string, timeout time.Duration) ([]map[string]string, error) {
	raw, err := runGit(root, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, timeout, "reading repository status", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseStatus(raw)
}

func indexEntries(root string, paths []string, timeout time.Duration) (map[string][]string, error) {
	result := make(map[string][]string, len(paths))
	for _, value := range paths {
		result[value] = []string{}
	}
	pathspecs := literalPathspecs(paths)
	for start := 0; start < len(pathspecs); {
		end, size := start, 0
		for end < len(pathspecs) {
			argumentBytes := len([]byte(pathspecs[end])) + 1
			if end > start && size+argumentBytes > indexPathspecByteBudget {
				break
			}
			size += argumentBytes
			end++
		}
		args := append([]string{"ls-files", "--stage", "-z", "--"}, pathspecs[start:end]...)
		raw, err := runGit(root, args, timeout, "fingerprinting index entries", nil, nil)
		if err != nil {
			return nil, err
		}
		for _, record := range bytes.Split(raw, []byte{0}) {
			if len(record) == 0 {
				continue
			}
			separator := strings.IndexByte(string(record), '\t')
			if separator < 0 {
				return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git index output was malformed."}
			}
			metadata, name := string(record[:separator]), decodeGitPath(record[separator+1:])
			ascii := true
			for _, value := range record[:separator] {
				if value > 127 {
					ascii = false
					break
				}
			}
			if !ascii || len(strings.Split(metadata, " ")) != 3 {
				return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git index output was malformed."}
			}
			result[name] = append(result[name], metadata)
		}
		start = end
	}
	for name := range result {
		sort.Strings(result[name])
	}
	return result, nil
}

func decodeGitPath(value []byte) string { return strings.ReplaceAll(string(value), `\`, "/") }

func hiddenProcessAttributes() *syscall.SysProcAttr {
	attributes := &syscall.SysProcAttr{}
	if runtime.GOOS == "windows" {
		value := reflect.ValueOf(attributes).Elem()
		if field := value.FieldByName("HideWindow"); field.IsValid() && field.CanSet() {
			field.SetBool(true)
		}
		if field := value.FieldByName("CreationFlags"); field.IsValid() && field.CanSet() {
			field.SetUint(0x08000000)
		}
	}
	return attributes
}
