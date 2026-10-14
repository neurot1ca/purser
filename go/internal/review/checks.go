package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

type CheckResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	DurationMS int    `json:"duration_ms"`
	Tail       string `json:"tail"`
}

type ChecksReport struct {
	Status          string        `json:"status"`
	Results         []CheckResult `json:"results"`
	SideEffectPaths []string      `json:"side_effect_paths"`
}

// SelectChecks applies the profile's protected-path glob semantics.
func SelectChecks(checks []profile.Check, changedPaths []string) []profile.Check {
	selected := make([]profile.Check, 0, len(checks))
	for _, check := range checks {
		if check.When == nil || anyProtected(check.When, changedPaths) {
			selected = append(selected, check)
		}
	}
	return selected
}

func anyProtected(patterns, paths []string) bool {
	for _, path := range paths {
		if profile.IsProtected(path, patterns) {
			return true
		}
	}
	return false
}

// RunChecks uses one context deadline for the ordered check set. The context also
// carries caller cancellation; each check's timeout can shorten that deadline.
func RunChecks(repoRoot string, checks []profile.Check, deadline context.Context) []CheckResult {
	return RunChecksReport(repoRoot, checks, deadline).Results
}

func RunChecksReport(repoRoot string, checks []profile.Check, deadline context.Context) ChecksReport {
	results := make([]CheckResult, 0, len(checks))
	if len(checks) == 0 {
		return ChecksReport{Status: "NOT_RUN", Results: results, SideEffectPaths: []string{}}
	}
	before := checkStatus(repoRoot)
	var fingerprints map[string]fileFingerprint
	if before != nil {
		fingerprints = checkFingerprints(repoRoot, before)
	}
	for _, check := range checks {
		if deadline.Err() != nil {
			results = append(results, CheckResult{Name: check.Name, Status: "UNKNOWN", Tail: "not run: deadline"})
			continue
		}
		results = append(results, runCheck(repoRoot, check, deadline))
	}
	after := checkStatus(repoRoot)
	var paths []string
	if before == nil || after == nil {
		paths = nil
	} else {
		paths = compareCheckMutations(repoRoot, before, after, fingerprints)
	}
	return ChecksReport{Status: AggregateCheckStatus(results), Results: results, SideEffectPaths: paths}
}

func runCheck(repoRoot string, check profile.Check, deadline context.Context) CheckResult {
	started := time.Now()
	result := CheckResult{Name: check.Name}
	if len(check.Argv) == 0 {
		result.Status, result.Tail = "UNKNOWN", "launch error: argv is empty"
		return result
	}
	executable, err := exec.LookPath(check.Argv[0])
	if err != nil {
		result.Status, result.Tail = "UNKNOWN", tail("unresolvable executable: "+check.Argv[0])
		return result
	}
	ctx, cancel := context.WithTimeout(deadline, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := proc.Hide(exec.Command(executable, check.Argv[1:]...))
	cmd.Dir = repoRoot
	for _, value := range cmd.Environ() {
		if !strings.HasPrefix(value, "CI=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CI=1")
	var output bytes.Buffer
	rootExit, err := runSupervised(ctx, cmd.Args, cmd.Dir, cmd.Environ(), nil, &output, &output)
	result.DurationMS = int(time.Since(started).Milliseconds())
	code := rootExit
	switch {
	case ctx.Err() != nil:
		result.Status, result.Tail = "UNKNOWN", tail(output.String()+"\ntimed out")
	case errors.Is(err, errTeardownUnconfirmed):
		result.Status, result.Tail = "UNKNOWN", tail(output.String()+"\nprocess-tree teardown unconfirmed")
	case rootExit == 0 && err == nil:
		result.Status, result.ExitCode = "PASS", &code
	case rootExit >= 0:
		result.Status, result.ExitCode, result.Tail = "FAIL", &code, tail(output.String())
	default:
		result.Status, result.Tail = "UNKNOWN", tail("launch error: "+err.Error())
	}
	return result
}

func tail(output string) string {
	output = strings.ToValidUTF8(output, "\uFFFD")
	runes := []rune(output)
	if len(runes) > 1500 {
		runes = runes[len(runes)-1500:]
	}
	return string(runes)
}

type fileFingerprint struct {
	size   int64
	digest [32]byte
}

func checkStatus(repoRoot string) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := proc.Hide(exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all"))
	cmd.Dir, cmd.Stderr = repoRoot, io.Discard
	proc.ConfigureNoWindow(cmd)
	data, err := cmd.Output()
	if err != nil {
		return nil
	}
	records := strings.Split(strings.ToValidUTF8(string(data), "\uFFFD"), "\x00")
	statuses := make(map[string]string)
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) >= 4 && record[2] == ' ' {
			statuses[record[3:]] = record[:2]
			if strings.ContainsAny(record[:2], "RC") {
				i++
			}
		}
	}
	return statuses
}
func compareCheckMutations(repoRoot string, before, after map[string]string, fingerprints map[string]fileFingerprint) []string {
	sideEffects := make(map[string]bool)
	for path, status := range before {
		if after[path] != status {
			sideEffects[path] = true
		}
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			sideEffects[path] = true
		}
	}
	for path, fingerprint := range fingerprints {
		if checkFingerprintChanged(repoRoot, path, fingerprint) {
			sideEffects[path] = true
		}
	}
	paths := make([]string, 0, len(sideEffects))
	for path := range sideEffects {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > 50 {
		paths = paths[:50]
	}
	return paths
}
func checkFingerprints(repoRoot string, statuses map[string]string) map[string]fileFingerprint {
	fingerprints := make(map[string]fileFingerprint)
	var total, read int64
	for relative := range statuses {
		path := filepath.Join(repoRoot, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if info.IsDir() {
			continue
		}
		total += info.Size()
		if len(fingerprints) == 500 || total > 64*1024*1024 {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, io.LimitReader(file, 64*1024*1024-read+1))
		closeErr := file.Close()
		read += size
		if copyErr != nil || closeErr != nil || read > 64*1024*1024 || size != info.Size() {
			return nil
		}
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		fingerprints[relative] = fileFingerprint{size: size, digest: digest}
	}
	return fingerprints
}

func checkFingerprintChanged(repoRoot, relative string, before fileFingerprint) bool {
	path := filepath.Join(repoRoot, filepath.FromSlash(relative))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() != before.size {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		return true
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	return copyErr != nil || closeErr != nil || size != before.size || string(hash.Sum(nil)) != string(before.digest[:])
}

func AggregateCheckStatus(results []CheckResult) string {
	if len(results) == 0 {
		return "NOT_RUN"
	}
	status := "PASS"
	for _, result := range results {
		if result.Status == "FAIL" {
			return "FAIL"
		}
		if result.Status == "UNKNOWN" {
			status = "UNKNOWN"
		}
	}
	return status
}
