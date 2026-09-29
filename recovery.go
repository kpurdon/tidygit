package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// archiveStaleWorktree preserves Git history, staging state, and the complete
// directory before removing its registration. An error always retains either
// the original worktree or its relocated files; callers must not delete its branch.
func (r repository) archiveStaleWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool) (archive string, err error) {
	if os.Getenv("GIT_INDEX_FILE") != "" {
		return "", errors.New("unset GIT_INDEX_FILE before archiving a stale worktree")
	}
	if err = r.checkWorktreeIdentity(ctx, wt, b, hasBranch); err != nil {
		return "", err
	}
	sourceRoot, err := os.OpenRoot(filepath.Dir(wt.Path))
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	sourceInfo, err := sourceRoot.Lstat(filepath.Base(wt.Path))
	if err != nil {
		return "", err
	}
	if !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("worktree is not a real directory")
	}
	linked := r
	linked.Dir = wt.Path
	plan, err := r.submoduleRecoveryPlan(ctx, wt)
	if err != nil {
		return "", err
	}
	gitDir, err := linked.gitDir(ctx)
	if err != nil {
		return "", err
	}
	var adminParent, ownedAdmin *os.Root
	var adminInfo os.FileInfo
	if plan.hasSubmodules {
		adminParent, err = os.OpenRoot(filepath.Dir(plan.admin))
		if err != nil {
			return "", err
		}
		defer adminParent.Close()
		adminInfo, err = adminParent.Lstat(filepath.Base(plan.admin))
		if err != nil {
			return "", err
		}
		ownedAdmin, err = adminParent.OpenRoot(filepath.Base(plan.admin))
		if err != nil {
			return "", err
		}
		defer ownedAdmin.Close()
	}
	// Git writers honor index.lock. Holding it keeps the staged snapshot stable
	// while files are relocated; ordinary file writes remain part of the archive.
	lockPath := filepath.Join(gitDir, "index.lock")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("lock worktree index: %w", err)
	}
	defer lock.Close()
	defer func() {
		held, heldErr := lock.Stat()
		if ownedAdmin != nil {
			current, currentErr := ownedAdmin.Lstat("index.lock")
			if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
				_ = ownedAdmin.Remove("index.lock")
			}
			return
		}
		current, currentErr := os.Lstat(lockPath)
		if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
			_ = os.Remove(lockPath)
		}
	}()
	index, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		return "", err
	}
	shallow, err := r.git(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(shallow) != "false" {
		return "", errors.New("cannot create complete recovery history from a shallow repository")
	}
	state, err := stateDirectory()
	if err != nil {
		return "", err
	}
	base := filepath.Join(state, "archives")
	if err = recoveryOutsideWorktree(wt.Path, base); err != nil {
		return "", err
	}
	if err = os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	archive, err = os.MkdirTemp(base, "stale-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("stale-worktree archival stopped; original files or recovery archive %q remain: %w", archive, err)
		}
	}()
	if err = os.WriteFile(filepath.Join(archive, "index.original"), index, 0o600); err != nil {
		return archive, err
	}
	indexCopy := filepath.Join(archive, "index")
	if err = os.WriteFile(indexCopy, index, 0o600); err != nil {
		return archive, err
	}
	// A split index references a shared file in the original Git directory. Git
	// expands that dependency into the copy before the registration can disappear.
	if _, err = linked.recoveryGit(ctx, indexCopy, "update-index", "--no-split-index"); err != nil {
		return archive, err
	}
	tree, err := linked.recoveryGit(ctx, indexCopy, "write-tree")
	if err != nil {
		return archive, err
	}
	snapshot, err := linked.git(ctx, "-c", "user.name=tidygit", "-c", "user.email=tidygit@localhost", "-c", "commit.gpgsign=false", "commit-tree", strings.TrimSpace(tree), "-p", wt.OID, "-m", "tidygit archived staging snapshot")
	if err != nil {
		return archive, err
	}
	snapshot = strings.TrimSpace(snapshot)
	ref := "refs/tidygit/" + filepath.Base(archive)
	if _, err = r.git(ctx, "update-ref", ref, snapshot, strings.Repeat("0", len(snapshot))); err != nil {
		return archive, err
	}
	defer func() {
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, cleanupErr := r.git(cleanupCtx, "update-ref", "-d", ref, snapshot)
		err = errors.Join(err, cleanupErr)
	}()
	bundle := filepath.Join(archive, "history.bundle")
	if _, err = linked.recoveryGit(ctx, "", "bundle", "create", bundle, "HEAD", ref); err != nil {
		return archive, err
	}
	if _, err = linked.recoveryGit(ctx, "", "bundle", "verify", bundle); err != nil {
		return archive, err
	}
	var readme strings.Builder
	fmt.Fprintf(&readme, "Original worktree (Go-quoted): %q\nBranch (Go-quoted): %q\nOriginal HEAD: %s\nStaging snapshot: %s\nStaging ref: %s\n\nThe complete original worktree is in files/%s, including ignored files and symlinks. The old .git pointer is preserved separately as gitdir.original; do not restore it.\n\nRecovery in a NEW directory:\n1. Run git init, then git fetch /absolute/path/to/history.bundle 'refs/tidygit/*:refs/tidygit/*' HEAD.\n2. Run git checkout --detach %s.\n3. Replace the new checkout contents (except .git) with the contents of files/%s, excluding its .git file. Preserve symlinks and permissions.\n4. Copy this archive's index file to the new checkout's .git/index.\nThis restores staged versus unstaged changes, deletions, untracked files, and ignored files. index.original is the untouched original index; index is the self-contained recovery copy.\nNever overwrite a directory containing other work. If archival failed, files may still be at the original path.\n", wt.Path, wt.Branch, wt.OID, snapshot, ref, filepath.Base(wt.Path), wt.OID, filepath.Base(wt.Path))
	if plan.hasSubmodules {
		readme.WriteString("\nSubmodule recovery:\nThe complete owned worktree metadata is preserved in git-admin, including submodule objects, refs, stashes, reflogs, and indexes. This directory is data, not a usable parent repository: its commondir points to the old repository. Original child .git pointers are stored separately under submodule-gitfiles; never restore those old pointers.\nFor each initialized child below, copy its Git directory from git-admin to the child's .git directory in the recovered checkout, then run git config --file <child/.git/config> --unset-all core.worktree (if present). Preserve the existing child files and index; do not run checkout or reset. Repeat for nested children. Metadata for removed/deinitialized children remains under git-admin/modules; its config core.worktree records the original checkout location.\n")
		for _, path := range slices.Sorted(maps.Keys(plan.modules)) {
			rel, _ := filepath.Rel(plan.admin, plan.modules[path])
			fmt.Fprintf(&readme, "Child path (Go-quoted): %q; archived Git directory (Go-quoted): %q\n", path, filepath.Join("git-admin", rel))
		}
		readme.WriteString("If archival stopped after moving files but before moving metadata, the original Git admin directory remains locked to prevent pruning. Recover it before removing that lock.\n")
	}
	if err = os.WriteFile(filepath.Join(archive, "README.txt"), []byte(readme.String()), 0o600); err != nil {
		return archive, err
	}
	if err = r.checkWorktreeIdentity(ctx, wt, b, hasBranch); err != nil {
		return archive, err
	}
	currentIndex, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		return archive, err
	}
	if !bytes.Equal(index, currentIndex) {
		return archive, errors.New("worktree index changed during archival")
	}
	currentInfo, err := sourceRoot.Lstat(filepath.Base(wt.Path))
	if err != nil {
		return archive, err
	}
	if !os.SameFile(sourceInfo, currentInfo) {
		return archive, errors.New("worktree directory changed during archival")
	}
	var protection *os.File
	filesMoved, adminMoved := false, false
	if plan.hasSubmodules {
		freshPlan, policyErr := r.submoduleRecoveryPlan(ctx, wt)
		if policyErr != nil {
			return archive, policyErr
		}
		if freshPlan.admin != plan.admin || !maps.Equal(freshPlan.modules, plan.modules) {
			return archive, errors.New("submodule metadata changed during archival")
		}
		currentAdmin, statErr := adminParent.Lstat(filepath.Base(plan.admin))
		if statErr != nil {
			return archive, statErr
		}
		if !os.SameFile(adminInfo, currentAdmin) {
			return archive, errors.New("owned worktree metadata changed during archival")
		}
		protection, err = ownedAdmin.OpenFile("locked", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return archive, fmt.Errorf("protect owned submodule metadata from pruning: %w", err)
		}
		defer protection.Close()
		defer func() {
			if filesMoved && !adminMoved {
				return
			}
			held, heldErr := protection.Stat()
			current, currentErr := ownedAdmin.Lstat("locked")
			if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
				_ = ownedAdmin.Remove("locked")
			}
		}()
		if _, err = protection.WriteString("tidygit recovery in progress; preserve Git metadata until archive recovery is complete\n"); err != nil {
			return archive, err
		}
	}
	sourceParent, err := sourceRoot.Open(".")
	if err != nil {
		return archive, err
	}
	defer sourceParent.Close()
	files := filepath.Join(archive, "files")
	if err = os.Mkdir(files, 0o700); err != nil {
		return archive, err
	}
	destinationParent, err := os.Open(files)
	if err != nil {
		return archive, err
	}
	defer destinationParent.Close()
	if err = ctx.Err(); err != nil {
		return archive, err
	}
	if err = renamePinnedArchive(sourceParent, destinationParent, filepath.Base(wt.Path)); err != nil {
		return archive, fmt.Errorf("move whole worktree (archive must share its filesystem): %w", err)
	}
	filesMoved = true
	movedInfo, err := os.Lstat(filepath.Join(files, filepath.Base(wt.Path)))
	if err != nil {
		return archive, err
	}
	if !os.SameFile(sourceInfo, movedInfo) {
		return archive, errors.New("worktree directory changed while being moved")
	}
	if _, statErr := sourceRoot.Lstat(filepath.Base(wt.Path)); !errors.Is(statErr, os.ErrNotExist) {
		return archive, fmt.Errorf("original worktree path reappeared or could not be checked: %v", statErr)
	}
	if plan.hasSubmodules {
		staging := filepath.Join(archive, "admin-relocation")
		if err = os.Mkdir(staging, 0o700); err != nil {
			return archive, err
		}
		target, openErr := os.Open(staging)
		if openErr != nil {
			return archive, openErr
		}
		defer target.Close()
		source, openErr := adminParent.Open(".")
		if openErr != nil {
			return archive, openErr
		}
		defer source.Close()
		if err = renamePinnedArchive(source, target, filepath.Base(plan.admin)); err != nil {
			return archive, fmt.Errorf("preserve owned Git metadata (original remains locked): %w", err)
		}
		adminMoved = true
		movedAdmin, statErr := os.Lstat(filepath.Join(staging, filepath.Base(plan.admin)))
		if statErr != nil {
			return archive, statErr
		}
		if !os.SameFile(adminInfo, movedAdmin) {
			return archive, errors.New("owned metadata changed during relocation; preserve both original and archived directories")
		}
	} else {
		// Never force-delete a directory that races into the now-missing old path.
		if _, err = r.git(ctx, "worktree", "remove", "--", wt.Path); err != nil {
			return archive, err
		}
	}
	archiveRoot, err := os.OpenRoot(archive)
	if err != nil {
		return archive, err
	}
	defer archiveRoot.Close()
	if plan.hasSubmodules {
		if err = archiveRoot.Rename(filepath.Join("admin-relocation", filepath.Base(plan.admin)), "git-admin"); err != nil {
			return archive, err
		}
		if err = archiveRoot.Remove("admin-relocation"); err != nil {
			return archive, err
		}
		// Each stale pointer could otherwise reconnect to a newly reused admin path.
		for _, path := range slices.Sorted(maps.Keys(plan.modules)) {
			saved := filepath.Join("submodule-gitfiles", path, "gitdir.original")
			if err = archiveRoot.MkdirAll(filepath.Dir(saved), 0o700); err != nil {
				return archive, err
			}
			if err = archiveRoot.Rename(filepath.Join("files", filepath.Base(wt.Path), path, ".git"), saved); err != nil {
				return archive, err
			}
		}

	}
	// The old admin path may be reused by a future worktree. Keep its pointer as
	// recovery metadata, never as an active .git file inside the archived payload.
	if err = archiveRoot.Rename(filepath.Join("files", filepath.Base(wt.Path), ".git"), "gitdir.original"); err != nil {
		return archive, err
	}
	return archive, nil
}

func (r repository) recoveryGit(ctx context.Context, index string, args ...string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1")
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("git recovery %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func recoveryOutsideWorktree(worktree, base string) error {
	source := canonicalPath(worktree)
	candidate, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			candidate = filepath.Join(append([]string{resolved}, suffix...)...)
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return resolveErr
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return resolveErr
		}
		suffix = append([]string{filepath.Base(candidate)}, suffix...)
		candidate = parent
	}
	if rel, err := filepath.Rel(source, candidate); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return errors.New("recovery archive must be outside the worktree")
	}
	return nil
}

// archiveBranchHistory saves the exact branch tip and its ancestry before the
// caller attempts a separately guarded branch deletion.
func (r repository) archiveBranchHistory(ctx context.Context, b Branch) (archive string, err error) {
	if err = r.checkBranch(ctx, b); err != nil {
		return "", err
	}
	shallow, err := r.git(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(shallow) != "false" {
		return "", errors.New("cannot create complete recovery history from a shallow repository")
	}
	state, err := stateDirectory()
	if err != nil {
		return "", err
	}
	base := filepath.Join(state, "archives")
	if err = recoveryOutsideWorktree(r.Dir, base); err != nil {
		return "", err
	}
	if err = os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	archive, err = os.MkdirTemp(base, "branch-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("branch archive %q incomplete; branch retained: %w", archive, err)
		}
	}()
	bundle := filepath.Join(archive, "history.bundle")
	if _, err = r.recoveryGit(ctx, "", "bundle", "create", bundle, "refs/heads/"+b.Name); err != nil {
		return archive, err
	}
	if _, err = r.recoveryGit(ctx, "", "bundle", "verify", bundle); err != nil {
		return archive, err
	}
	heads, err := r.recoveryGit(ctx, "", "bundle", "list-heads", bundle)
	if err != nil {
		return archive, err
	}
	if !strings.Contains(heads, b.OID+" refs/heads/"+b.Name+"\n") {
		return archive, errors.New("branch changed while creating bundle")
	}
	if err = r.checkBranch(ctx, b); err != nil {
		return archive, err
	}
	readme := fmt.Sprintf("Original repository (Go-quoted): %q\nBranch (Go-quoted): %q\nArchived commit: %s\n\nThe history.bundle contains this commit and its complete ancestry.\nTo recover into a NEW repository, run git init, fetch this bundle's branch ref, and checkout the archived commit shown above.\nDo not overwrite existing work.\n", r.Dir, b.Name, b.OID)
	if err = os.WriteFile(filepath.Join(archive, "README.txt"), []byte(readme), 0o600); err != nil {
		return archive, err
	}
	return archive, nil
}
