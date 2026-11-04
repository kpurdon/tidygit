package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type submoduleDisposal struct {
	submoduleRecovery
	fingerprint string
}

// checkDisposableSubmodules verifies recoverable history or disposable remote
// cache without fetching or changing local refs, indexes, or worktrees.
func (r repository) checkDisposableSubmodules(ctx context.Context, wt Worktree) error {
	_, err := r.disposableSubmodulePlan(ctx, wt)
	return err
}

func (r repository) disposableSubmodulePlan(ctx context.Context, wt Worktree) (submoduleDisposal, error) {
	recovery, err := r.submoduleRecoveryPlan(ctx, wt)
	plan := submoduleDisposal{submoduleRecovery: recovery}
	if err != nil {
		return plan, err
	}
	if err := checkInitializedModuleStorage(recovery); err != nil {
		return plan, err
	}
	// Capture every child before the first remote lookup can give another
	// process time to create local history in an already-checked child.
	plan.fingerprint, err = r.submoduleDisposalFingerprint(ctx, wt, recovery)
	if err != nil {
		return plan, err
	}
	for _, path := range slices.Sorted(maps.Keys(plan.modules)) {
		admin := plan.modules[path]
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
	return plan, r.checkSubmoduleDisposalSnapshot(ctx, wt, plan)
}

func (r repository) revalidateSubmoduleDisposal(ctx context.Context, wt Worktree, plan submoduleDisposal) error {
	current, err := r.submoduleRecoveryPlan(ctx, wt)
	if err != nil {
		return err
	}
	if current.admin != plan.admin || current.hasSubmodules != plan.hasSubmodules || !maps.Equal(current.modules, plan.modules) {
		return errors.New("submodule ownership or initialization changed during verification")
	}
	if err := checkInitializedModuleStorage(current); err != nil {
		return err
	}
	return r.checkSubmoduleDisposalSnapshot(ctx, wt, plan)
}

func (r repository) checkSubmoduleDisposalSnapshot(ctx context.Context, wt Worktree, plan submoduleDisposal) error {
	fingerprint, err := r.submoduleDisposalFingerprint(ctx, wt, plan.submoduleRecovery)
	if err != nil {
		return err
	}
	if fingerprint != plan.fingerprint {
		return errors.New("submodule recovery data changed during verification; keeping the entire checkout")
	}
	return nil
}

func (r repository) submoduleDisposalFingerprint(ctx context.Context, wt Worktree, plan submoduleRecovery) (string, error) {
	admin, err := r.ownedWorktreeAdmin(ctx, wt)
	if err != nil {
		return "", err
	}
	if admin != plan.admin {
		return "", errors.New("worktree Git storage changed during verification")
	}
	h := sha256.New()
	for _, path := range slices.Sorted(maps.Keys(plan.modules)) {
		child := r
		child.Dir = filepath.Join(wt.Path, path)
		if err := child.checkDisposableExtensionStorage(ctx, plan.modules[path]); err != nil {
			return "", fmt.Errorf("submodule %q: %w", path, err)
		}
		fingerprintField(h, path)
		fingerprintField(h, plan.modules[path])
		pointer := filepath.Join(child.Dir, ".git")
		info, err := os.Lstat(pointer)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("submodule Git pointer is no longer a regular file")
		}
		if err := fingerprintTree(ctx, h, pointer, true); err != nil {
			return "", err
		}
	}
	root := filepath.Join(plan.admin, "modules")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		fingerprintField(h, "modules:absent")
	} else if err != nil {
		return "", err
	} else if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".lock") {
			return errors.New("submodule Git metadata became symlinked or locked")
		}
		fingerprintField(h, path)
		fingerprintField(h, info.Mode().String())
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("submodule Git metadata contains an unsupported file type")
		}
		for _, admin := range plan.modules {
			if strings.HasPrefix(path, filepath.Join(admin, "objects")+string(filepath.Separator)) {
				// Git objects and packs are immutable; ordinary Git writes create
				// new paths. Mutable refs, indexes, and reflogs are hashed below.
				fingerprintField(h, fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()))
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fingerprintField(h, string(data))
		return nil
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (r repository) checkDisposableExtensionStorage(ctx context.Context, admin string) error {
	entries, err := os.ReadDir(admin)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "HEAD", "ORIG_HEAD", "FETCH_HEAD", "config", "config.worktree", "description", "index", "packed-refs", "shallow", "gitdir", "commondir", "COMMIT_EDITMSG", "AUTO_MERGE":
			if !entry.Type().IsRegular() {
				return errors.New("unsupported child Git metadata requires preservation")
			}
		case "branches", "hooks", "info", "logs", "modules", "objects", "refs", "reftable":
			if !entry.IsDir() {
				return errors.New("unsupported child Git metadata requires preservation")
			}
		default:
			return errors.New("child Git extension or unsupported storage requires preservation")
		}
	}
	configured, err := r.disposalGit(ctx, "", "config", "--name-only", "--get-regexp", `^(lfs\.|annex\.|svn-remote\.|git-p4\.)`)
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 {
			return err
		}
	} else if configured != "" {
		return errors.New("child Git extension storage is configured and requires preservation")
	}
	return nil
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
	refs, err := r.disposalGit(ctx, "", "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)")
	if err != nil {
		return err
	}
	stale := map[string]string{}
	protected := []string{recorded}
	for line := range strings.SplitSeq(refs, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !disposalObjectID(fields[1]) {
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
		branch, origin := strings.CutPrefix(name, "refs/remotes/origin/")
		if origin && branch != "HEAD" && fields[2] == "" && remoteRefs["refs/heads/"+branch] == "" {
			stale[name] = oid
		} else {
			protected = append(protected, oid)
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
	if len(objects) != 0 && len(stale) != 0 {
		if err := r.excludeStaleRemoteCache(ctx, admin, stale, protected, objects); err != nil {
			return err
		}
	}
	if len(objects) != 0 {
		return fmt.Errorf("%d stored child objects are not recoverable from live origin refs", len(objects))
	}
	return nil
}

// Conventional origin-tracking refs are cache, including clone-created refs
// without reflogs. Manual ref activity and local reflogs protect both endpoints,
// including the previous commit of the oldest retained entry.
func (r repository) excludeStaleRemoteCache(ctx context.Context, admin string, stale map[string]string, protected []string, objects map[string]bool) error {
	fetch, err := r.disposalGit(ctx, "", "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		return err
	}
	if strings.TrimSpace(fetch) != "+refs/heads/*:refs/remotes/origin/*" {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(admin, "reftable")); err == nil {
		return errors.New("reftable reflogs require preservation of stale remote history")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{"ORIG_HEAD", "AUTO_MERGE"} {
		data, err := os.ReadFile(filepath.Join(admin, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		oid := strings.TrimSpace(string(data))
		if !disposalObjectID(oid) {
			return fmt.Errorf("unable to inspect child %s", name)
		}
		protected = append(protected, oid)
	}
	logs := filepath.Join(admin, "logs")
	if err := filepath.WalkDir(logs, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == logs {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("unsupported child reflog storage")
		}
		name, err := filepath.Rel(logs, path)
		if err != nil {
			return err
		}
		cacheLog := stale[filepath.ToSlash(name)] != ""
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || !disposalObjectID(fields[0]) || !disposalObjectID(fields[1]) {
				return errors.New("unable to inspect child reflog endpoints")
			}
			_, message, _ := strings.Cut(line, "\t")
			if cacheLog && (strings.HasPrefix(message, "fetch ") || strings.HasPrefix(message, "fetch:") || strings.HasPrefix(message, "clone: from ")) {
				continue
			}
			for _, oid := range fields[:2] {
				if strings.Trim(oid, "0") != "" {
					protected = append(protected, oid)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	cache, err := r.disposalGit(ctx, strings.Join(slices.Collect(maps.Values(stale)), "\n")+"\n", "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return fmt.Errorf("unable to inspect stale remote history: %w", err)
	}
	local, err := r.disposalGit(ctx, strings.Join(protected, "\n")+"\n", "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return fmt.Errorf("unable to inspect protected child history: %w", err)
	}
	protectedObjects := map[string]bool{}
	for oid := range strings.FieldsSeq(local) {
		protectedObjects[oid] = true
	}
	for oid := range strings.FieldsSeq(cache) {
		if !protectedObjects[oid] {
			delete(objects, oid)
		}
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
