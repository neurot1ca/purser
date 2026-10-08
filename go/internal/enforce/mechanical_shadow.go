package enforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

const mechanicalShadowTimeout = 45 * time.Second

type mechanicalShadowJob struct {
	Model      string
	Prompt     string
	Repository string
	Scratch    string
}

type mechanicalShadowOutcome struct {
	Stdout   string
	ExitCode int
	TimedOut bool
}

func runMechanicalShadow(ctx context.Context, job mechanicalShadowJob, executable string, timeout time.Duration) (mechanicalShadowOutcome, error) {
	var outcome mechanicalShadowOutcome
	source, err := filepath.Abs(job.Repository)
	if err != nil {
		return outcome, err
	}
	scratch, err := filepath.Abs(job.Scratch)
	if err != nil {
		return outcome, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return outcome, err
	}
	if pathsOverlap(source, scratch) {
		return outcome, fmt.Errorf("mechanical shadow scratch overlaps repository")
	}
	scratch, err = resolveMechanicalPath(scratch)
	if err != nil {
		return outcome, err
	}
	if pathsOverlap(source, scratch) {
		return outcome, fmt.Errorf("mechanical shadow scratch overlaps repository")
	}
	if executable == "" {
		executable, err = exec.LookPath("opencode")
		if err != nil {
			return outcome, err
		}
	}
	if timeout <= 0 {
		timeout = mechanicalShadowTimeout
	}
	if err = os.MkdirAll(filepath.Dir(scratch), 0700); err != nil {
		return outcome, err
	}
	if err = os.Mkdir(scratch, 0700); err != nil {
		return outcome, err
	}
	defer os.RemoveAll(scratch)
	if err = copyMechanicalShadow(source, scratch); err != nil {
		return outcome, err
	}
	permissions := map[string]string{"edit": "ask", "write": "ask", "bash": "ask", "webfetch": "ask", "task": "ask", "external_directory": "ask"}
	config, err := json.Marshal(map[string]any{
		"permission": permissions,
		"agent":      map[string]any{"purser-shadow": map[string]any{"mode": "primary", "permission": permissions}},
	})
	if err != nil {
		return outcome, err
	}
	argv := []string{"run", "--format", "json", "--model", job.Model, "--agent", "purser-shadow", "--dir", scratch, job.Prompt}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, executable, argv...)
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG_CONTENT="+string(config))
	cmd.Stdin, cmd.Stderr = nil, io.Discard
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	proc.ConfigureProcessTreeKill(cmd)
	err = cmd.Run()
	outcome.Stdout = stdout.String()
	outcome.TimedOut = commandCtx.Err() == context.DeadlineExceeded
	if cmd.ProcessState != nil {
		outcome.ExitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		return outcome, err
	}
	return outcome, nil
}

func copyMechanicalShadow(source, scratch string) error {
	ignored := map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ignored[entry.Name()] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(scratch, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Mkdir(destination, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported mechanical shadow input: %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	})
}

func pathsOverlap(a, b string) bool {
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	return inside(a, b) || inside(b, a)
}

func resolveMechanicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := filepath.Dir(abs)
	for {
		if _, err = os.Stat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(ancestor, abs)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, rel), nil
}
