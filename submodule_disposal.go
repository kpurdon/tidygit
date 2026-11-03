package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// checkDisposableSubmodules proves recovery from currently advertised remote
// refs without fetching or changing local refs, indexes, or worktrees.
func (r repository) checkDisposableSubmodules(ctx context.Context, wt Worktree) error {
	_, err := r.disposableSubmodulePlan(ctx, wt)
	return err
}

func (r repository) disposableSubmodulePlan(ctx context.Context, wt Worktree) (submoduleRecovery, error) {
	plan, err := r.submoduleRecoveryPlan(ctx, wt)
	if err != nil {
		return plan, err
	}
	if err := checkInitializedModuleStorage(plan); err != nil {
		return plan, err
	}
	for path, admin := range plan.modules {
		child := r
		child.Dir = filepath.Join(wt.Path, path)
		parent := r
		parent.Dir = wt.Path
		name := path
		parentPath := ""
		for candidate := range plan.modules {
			if strings.HasPrefix(path, candidate+string(filepath.Separator)) && len(candidate) > len(parentPath) {
				parentPath = candidate
				parent.Dir = filepath.Join(wt.Path, candidate)
				name = strings.TrimPrefix(path, candidate+string(filepath.Separator))
			}
		}
		entry, err := parent.disposalGit(ctx, "", "ls-tree", "-z", "HEAD", "--", ":(literal)"+filepath.ToSlash(name))
		if err != nil {
			return plan, err
		}
		header, recordedPath, ok := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
		fields := strings.Fields(header)
		if !ok || recordedPath != filepath.ToSlash(name) || len(fields) != 3 || fields[0] != "160000" || fields[1] != "commit" {
			return plan, fmt.Errorf("submodule %q has no recorded parent gitlink", path)
		}
		if err := child.checkDisposableSubmodule(ctx, admin, fields[2]); err != nil {
			return plan, fmt.Errorf("submodule %q: %w", path, err)
		}
	}
	return plan, nil
}

func checkInitializedModuleStorage(plan submoduleRecovery) error {
	known := map[string]bool{}
	for _, admin := range plan.modules {
		known[admin] = true
	}
	var inspect func(string) error
	inspect = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if known[path] {
				if err := inspect(filepath.Join(path, "modules")); err != nil {
					return err
				}
				continue
			}
			container := false
			for admin := range known {
				if strings.HasPrefix(admin, path+string(filepath.Separator)) {
					container = true
					break
				}
			}
			if !entry.IsDir() || !container {
				return errors.New("unmapped or deinitialized submodule Git storage requires preservation")
			}
			if err := inspect(path); err != nil {
				return err
			}
		}
		return nil
	}
	return inspect(filepath.Join(plan.admin, "modules"))
}

func (r repository) checkDisposableSubmodule(ctx context.Context, admin, recorded string) error {
	if info, err := os.Stat(filepath.Join(admin, "info", "grafts")); err == nil && info.Size() > 0 {
		return errors.New("history grafts require preservation")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	head, err := r.disposalGit(ctx, "", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != recorded {
		return errors.New("HEAD differs from the recorded parent gitlink")
	}
	if err := r.checkVisibleIndex(ctx, r.Dir); err != nil {
		return err
	}
	status, err := r.disposalGit(ctx, "", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("modified or untracked child files require preservation")
	}
	if _, err := r.disposalGit(ctx, "", "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		return fmt.Errorf("child refs or object connectivity require preservation: %w", err)
	}
	remote, err := r.disposalGit(ctx, "", "ls-remote", "--refs", "--heads", "--tags", "origin")
	if err != nil {
		return fmt.Errorf("unable to verify live origin branches and tags: %w", err)
	}
	remoteRefs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(remote), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !disposalObjectID(fields[0]) || !(strings.HasPrefix(fields[1], "refs/heads/") || strings.HasPrefix(fields[1], "refs/tags/")) {
			return errors.New("origin did not advertise verifiable branches and tags")
		}
		remoteRefs[fields[1]] = fields[0]
	}
	refs, err := r.disposalGit(ctx, "", "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(refs), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !disposalObjectID(fields[1]) {
			return errors.New("unable to inspect local child refs")
		}
		name, oid := fields[0], fields[1]
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			if remoteRefs[name] == "" {
				return errors.New("local child branch has no matching live remote branch")
			}
		case strings.HasPrefix(name, "refs/tags/"):
			if remoteRefs[name] != oid {
				return errors.New("local child tag differs from live origin")
			}
		case strings.HasPrefix(name, "refs/remotes/"):
		default:
			return errors.New("stash or custom child refs require preservation")
		}
	}
	stored, err := r.disposalGit(ctx, "", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if err != nil {
		return err
	}
	objects := map[string]bool{}
	for oid := range strings.FieldsSeq(stored) {
		if !disposalObjectID(oid) {
			return errors.New("unable to enumerate stored child objects")
		}
		objects[oid] = true
	}
	anchors := []string{}
	for _, oid := range remoteRefs {
		if objects[oid] {
			anchors = append(anchors, oid)
		}
	}
	if len(anchors) == 0 {
		return errors.New("no advertised remote history is available locally for verification")
	}
	slices.Sort(anchors)
	anchors = slices.Compact(anchors)
	reachable, err := r.disposalGit(ctx, strings.Join(anchors, "\n")+"\n", "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return fmt.Errorf("unable to verify complete advertised remote history: %w", err)
	}
	for oid := range strings.FieldsSeq(reachable) {
		delete(objects, oid)
	}
	if len(objects) != 0 {
		return fmt.Errorf("%d stored child objects are not recoverable from live origin refs", len(objects))
	}
	return nil
}

func disposalObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (r repository) disposalGit(ctx context.Context, input string, args ...string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	args = append([]string{"-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1")
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("read-only submodule verification failed: %w", err)
	}
	return string(out), nil
}
