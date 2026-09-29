package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type repository struct {
	Dir, CommonDir, ProtectedPath string
	Timeout                       time.Duration
}

type Branch struct{ Name, OID, SourceRepo, SourceBranch, LocalRepo string }
type Worktree struct {
	Path, Branch, OID string
	Locked, Prunable  bool
}

func (r repository) command(ctx context.Context, name string, args ...string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
func (r repository) git(ctx context.Context, args ...string) (string, error) {
	return r.command(ctx, "git", args...)
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}
func discoverRepo(ctx context.Context, dir string, timeout time.Duration) (repository, error) {
	r := repository{Dir: canonicalPath(dir), Timeout: timeout}
	out, err := r.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return r, err
	}
	r.Dir = canonicalPath(strings.TrimSuffix(out, "\n"))
	out, err = r.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return r, err
	}
	r.CommonDir = canonicalPath(strings.TrimSuffix(out, "\n"))
	r.ProtectedPath = r.Dir
	return r, nil
}

func (r repository) defaultBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if target, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			name, ref, ok := strings.Cut(target, "\t")
			if ok && ref == "HEAD" && name != "" {
				return name, nil
			}
		}
	}
	return "", errors.New("origin does not advertise a default branch")
}
func (r repository) worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := r.git(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var result []Worktree
	current := Worktree{}
	for field := range strings.SplitSeq(out, "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "worktree":
			current.Path = value
		case "HEAD":
			current.OID = value
		case "branch":
			current.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "locked":
			current.Locked = true
		case "prunable":
			current.Prunable = true
		case "":
			if current.Path != "" {
				result = append(result, current)
				current = Worktree{}
			}
		}
	}
	if current.Path != "" {
		result = append(result, current)
	}
	return result, nil
}
func (r repository) branches(ctx context.Context) ([]Branch, error) {
	out, err := r.git(ctx, "for-each-ref", "--format=%(refname:strip=2)%00%(objectname)%00%(upstream:remotename)%00%(upstream:remoteref)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	remotes := map[string]string{}
	if raw, err := r.git(ctx, "remote", "get-url", "origin"); err == nil {
		remotes["origin"], _ = githubRepo(raw)
	}
	var result []Branch
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 4 {
			return nil, errors.New("invalid branch inventory")
		}
		b := Branch{Name: fields[0], OID: fields[1], SourceBranch: fields[0], LocalRepo: remotes["origin"]}
		remote := fields[2]
		if remote == "" {
			remote = "origin"
		}
		if remote != "." {
			identity, ok := remotes[remote]
			if !ok {
				url, err := r.git(ctx, "remote", "get-url", remote)
				if err != nil {
					return nil, err
				}
				identity, _ = githubRepo(strings.TrimSpace(url))
				remotes[remote] = identity
			}
			b.SourceRepo = identity
			if ref, ok := strings.CutPrefix(fields[3], "refs/heads/"); ok {
				b.SourceBranch = ref
			}
		}
		result = append(result, b)
	}
	return result, nil
}
func (r repository) status(ctx context.Context, path string, ignored bool) (string, error) {
	args := []string{"-C", path, "status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if ignored {
		args = append(args, "--ignored")
	}
	return r.git(ctx, args...)
}
func (r repository) checkBranch(ctx context.Context, b Branch) error {
	kept, err := r.keptBranches(ctx)
	if err != nil {
		return err
	}
	if kept[b.Name] {
		return fmt.Errorf("branch %s is explicitly retained by tidygit.keep", b.Name)
	}
	out, err := r.git(ctx, "rev-parse", "--verify", "refs/heads/"+b.Name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != b.OID {
		return fmt.Errorf("branch %s changed since discovery; keeping it", b.Name)
	}
	return nil
}
func (r repository) deleteBranch(ctx context.Context, b Branch) (bool, error) {
	if err := r.checkBranch(ctx, b); err != nil {
		return false, err
	}
	worktrees, err := r.worktrees(ctx)
	if err != nil {
		return false, err
	}
	for _, wt := range worktrees {
		if wt.Branch == b.Name {
			return false, fmt.Errorf("branch %s is checked out in %s; keeping it", b.Name, wt.Path)
		}
	}
	if state, err := r.worktreeOperation(ctx, worktrees); err != nil {
		return false, err
	} else if state != "" {
		return false, fmt.Errorf("%s; keeping branch %s", state, b.Name)
	}
	// Compare and delete under Git's ref lock so a newly updated tip is never discarded.
	if _, err := r.git(ctx, "update-ref", "--no-deref", "-d", "refs/heads/"+b.Name, b.OID); err != nil {
		return false, err
	}
	_, err = r.git(ctx, "config", "--local", "--get-regexp", "^branch\\."+regexp.QuoteMeta(b.Name)+"\\.")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
			return true, nil
		}
		return true, err
	}
	_, err = r.git(ctx, "config", "--local", "--remove-section", "branch."+b.Name)
	return true, err
}

func (r repository) protects(path string) bool {
	path = canonicalPath(path)
	return path == r.Dir || path == r.ProtectedPath
}

func (r repository) worktreeOperation(ctx context.Context, worktrees []Worktree) (string, error) {
	for _, wt := range worktrees {
		linked := r
		linked.Dir = wt.Path
		state, err := linked.inProgress(ctx)
		if err != nil {
			return "", err
		}
		if state != "" {
			return fmt.Sprintf("%s in progress in %s", state, wt.Path), nil
		}
	}
	return "", nil
}

func (r repository) gitDir(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Explicit Git environment overrides and unusual layouts remain Git's responsibility.
	if os.Getenv("GIT_DIR") == "" && os.Getenv("GIT_WORK_TREE") == "" {
		path := filepath.Join(r.Dir, ".git")
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				return path, nil
			}
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				if dir, ok := strings.CutPrefix(string(data), "gitdir: "); ok {
					// Git removes line endings, but whitespace and embedded newlines belong to the path.
					dir = strings.TrimRight(dir, "\r\n")
					if dir == "" {
						return "", fmt.Errorf("empty gitdir in %s", path)
					}
					if !filepath.IsAbs(dir) {
						dir = r.Dir + string(filepath.Separator) + dir
					}
					dir, err = filepath.EvalSymlinks(dir)
					if err != nil {
						return "", err
					}
					info, err := os.Stat(dir)
					if err != nil {
						return "", err
					}
					if !info.IsDir() {
						return "", fmt.Errorf("gitdir %s is not a directory", dir)
					}
					return dir, nil
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	out, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	return strings.TrimSuffix(out, "\n"), err
}

func (r repository) inProgress(ctx context.Context) (string, error) {
	dir, err := r.gitDir(ctx)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer", "BISECT_START"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}
