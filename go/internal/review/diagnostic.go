package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const reviewerDiagnosticBytes = 4 * 1024

var reviewerDiagnosticMu sync.Mutex

type reviewerDiagnostic struct {
	Provider       string   `json:"provider"`
	RequestedModel string   `json:"requested_model"`
	ErrorCode      string   `json:"error_code"`
	ExitCode       int      `json:"exit_code"`
	ElapsedSeconds float64  `json:"elapsed_seconds"`
	Retries        int      `json:"retries"`
	DecoderDetail  string   `json:"decoder_detail"`
	StdoutHead     string   `json:"stdout_head"`
	StdoutTail     string   `json:"stdout_tail"`
	StderrHead     string   `json:"stderr_head"`
	StderrTail     string   `json:"stderr_tail"`
	ExecutablePath string   `json:"executable_path"`
	ArgvFlags      []string `json:"argv_flags"`
	Cwd            string   `json:"cwd"`
	PathHead       string   `json:"path_head"`
}

func writeReviewerDiagnostic(provider, model, errorCode string, exitCode int, elapsed time.Duration, retries int, decoderDetail string, stdout []byte, stdoutTail, stderrHead, stderrTail, executablePath string, argvFlags []string, cwd, pathHead string) error {
	reviewerDiagnosticMu.Lock()
	defer reviewerDiagnosticMu.Unlock()

	dir := filepath.Join(reviewerDiagnosticStateRoot(), "diagnostics", "reviewer")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if stdoutTail == "" && len(stdout) > 0 {
		stdoutTail = diagnosticPart(stdout, true)
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + diagnosticProvider(provider) + ".json"
	diagnostic := reviewerDiagnostic{
		Provider: provider, RequestedModel: model, ErrorCode: errorCode, ExitCode: exitCode,
		ElapsedSeconds: elapsed.Seconds(), Retries: retries, DecoderDetail: decoderDetail,
		StdoutHead: diagnosticPart(stdout, false), StdoutTail: diagnosticPart([]byte(stdoutTail), false), StderrHead: stderrHead, StderrTail: stderrTail,
		ExecutablePath: executablePath, ArgvFlags: argvFlags, Cwd: cwd, PathHead: pathHead,
	}
	data, err := json.Marshal(diagnostic)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		return err
	}
	return trimReviewerDiagnostics(dir)
}

func diagnosticPart(value []byte, tail bool) string {
	if len(value) > reviewerDiagnosticBytes {
		if tail {
			value = value[len(value)-reviewerDiagnosticBytes:]
		} else {
			value = value[:reviewerDiagnosticBytes]
		}
	}
	return strings.ToValidUTF8(string(value), "�")
}

func reviewerDiagnosticStateRoot() string {
	if root := os.Getenv("PURSER_STATE_DIR"); root != "" {
		return root
	}
	if root := os.Getenv("CLAUDE_CONFIG_DIR"); root != "" {
		return filepath.Join(root, "purser")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "purser")
}

func diagnosticProvider(provider string) string {
	provider = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, provider)
	if provider == "" {
		return "unknown"
	}
	return provider
}

func trimReviewerDiagnostics(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	if len(files) <= 20 {
		return nil
	}
	for _, name := range files[20:] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
