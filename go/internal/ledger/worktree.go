package ledger

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/profile"
)

// CreateJobWorktree creates a detached worktree with the caller's non-ignored state.
// Callers must run configured setup afterward and must not clean up if setup fails.
func CreateJobWorktree(repoRoot, jobID, parentDir string) (map[string]string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.Abs(parentDir)
	if err != nil {
		return nil, err
	}
	if jobID == "" {
		return nil, &inspectionError{"INVALID_JOB_ID", "job_id must be a non-empty string"}
	}
	if err = os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	id := []rune(jobID)
	if len(id) > 12 {
		id = id[:12]
	}
	worktree := filepath.Join(parent, string(id))
	if _, err = os.Lstat(worktree); err == nil {
		return nil, os.ErrExist
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	const timeout = 30 * time.Second
	cleanup := func() {
		_, _ = runGit(root, []string{"worktree", "remove", "--force", worktree}, timeout, "removing failed job worktree", nil, nil)
		_ = os.RemoveAll(worktree)
		_, _ = runGit(root, []string{"worktree", "prune"}, timeout, "pruning failed job worktree", nil, nil)
	}
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	head, err := runGit(root, []string{"rev-parse", "--verify", "HEAD"}, timeout, "reading HEAD", nil, nil)
	if err != nil {
		return nil, err
	}
	headText := strings.TrimSpace(decodeGitPath(head))
	if _, err = runGit(root, []string{"worktree", "add", "--detach", worktree, headText}, timeout, "creating job worktree", nil, nil); err != nil {
		return nil, err
	}
	diff, err := runGit(root, []string{"diff", "--binary", "--no-ext-diff", "--no-textconv", "HEAD", "--"}, timeout, "capturing repository changes", nil, nil)
	if err != nil {
		return nil, err
	}
	if len(diff) > 0 {
		if _, err = runGit(worktree, []string{"apply", "--binary", "-"}, timeout, "reproducing tracked changes", diff, nil); err != nil {
			return nil, err
		}
	}
	if err = copyUntracked(root, worktree, timeout); err != nil {
		return nil, err
	}
	if _, err = runGit(worktree, []string{"add", "-A"}, timeout, "staging snapshot files", nil, nil); err != nil {
		return nil, err
	}
	snapshotRaw, err := runGit(worktree, []string{"write-tree"}, timeout, "writing snapshot tree", nil, nil)
	if err != nil {
		return nil, err
	}
	snapshot := strings.TrimSpace(decodeGitPath(snapshotRaw))
	resolved := profile.LoadProfile(root)
	if resolved.Trusted {
		for _, name := range resolved.Worktree.Copy {
			if err = copyProfilePath(root, worktree, name); err != nil {
				return nil, err
			}
		}
	}
	success = true
	return map[string]string{"path": worktree, "head": headText, "snapshot": snapshot}, nil
}

func copyUntracked(root, worktree string, timeout time.Duration) error {
	raw, err := runGit(root, []string{"ls-files", "--others", "--exclude-standard", "-z"}, timeout, "listing untracked files", nil, nil)
	if err != nil {
		return err
	}
	for _, item := range strings.Split(string(raw), "\x00") {
		if item == "" {
			continue
		}
		name := filepath.FromSlash(item)
		if !filepath.IsLocal(name) {
			return &inspectionError{"GIT_OUTPUT_INVALID", "Git returned an unsafe untracked path."}
		}
		if err = copyPath(filepath.Join(root, name), filepath.Join(worktree, name), false); err != nil {
			return err
		}
	}
	return nil
}
func copyProfilePath(root, worktree, raw string) error {
	normalized := strings.ReplaceAll(raw, `\`, "/")
	name := filepath.FromSlash(normalized)
	if normalized == "" || strings.ContainsRune(normalized, 0) || !filepath.IsLocal(name) || filepath.Clean(name) == "." {
		return &inspectionError{"INVALID_WORKTREE_COPY_PATH", "Unsafe worktree copy path: " + raw}
	}
	source := filepath.Join(root, name)
	resolved, err := filepath.EvalSymlinks(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &inspectionError{"INVALID_WORKTREE_COPY_PATH", "Unsafe worktree copy path: " + raw}
	}
	return copyPath(source, filepath.Join(worktree, name), true)
}
func copyPath(source, target string, recursive bool) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, e := os.Readlink(source)
		if e != nil {
			return e
		}
		return os.Symlink(link, target)
	}
	if info.IsDir() {
		if !recursive {
			return &inspectionError{"UNTRACKED_FILE_UNSUPPORTED", "An untracked path was not a regular file or symlink."}
		}
		entries, e := os.ReadDir(source)
		if e != nil {
			return e
		}
		if e = os.MkdirAll(target, info.Mode().Perm()); e != nil {
			return e
		}
		for _, entry := range entries {
			if e = copyPath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()), true); e != nil {
				return e
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return &inspectionError{"UNTRACKED_FILE_UNSUPPORTED", "An untracked path was not a regular file or symlink."}
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chtimes(target, info.ModTime(), info.ModTime())
}
