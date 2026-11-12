package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

// disposableWorktree requires clean source and recoverable submodule history.
// Ignored files in a verified-completed checkout are deliberately disposable.
func (r repository) disposableWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool) (submoduleDisposal, error) {
	if err := r.checkDisposableSource(ctx, wt, b, hasBranch); err != nil {
		return submoduleDisposal{}, err
	}
	plan, err := r.disposableSubmodulePlan(ctx, wt)
	if err != nil {
		return plan, err
	}
	return plan, r.checkDisposableSource(ctx, wt, b, hasBranch)
}

func (r repository) checkDisposableSource(ctx context.Context, wt Worktree, b Branch, hasBranch bool) error {
	if err := r.checkWorktreeIdentity(ctx, wt, b, hasBranch); err != nil {
		return err
	}
	if err := r.checkVisibleIndex(ctx, wt.Path); err != nil {
		return err
	}
	status, err := r.disposalGit(ctx, "", "-C", wt.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("worktree contains modified or untracked files; keeping it")
	}
	return nil
}

func (r repository) checkVisibleIndex(ctx context.Context, path string) error {
	out, err := r.disposalGit(ctx, "", "-C", path, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(out, "\x00") {
		if entry != "" && (entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z') {
			return errors.New("index hides local changes with skip-worktree or assume-unchanged")
		}
	}
	return nil
}

func (r repository) removeDisposableWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool) error {
	plan, err := r.disposableWorktree(ctx, wt, b, hasBranch)
	if err != nil {
		return err
	}
	if err := r.revalidateSubmoduleDisposal(ctx, wt, plan); err != nil {
		return err
	}
	if err := r.checkDisposableSource(ctx, wt, b, hasBranch); err != nil {
		return err
	}
	if err := r.checkSubmoduleDisposalSnapshot(ctx, wt, plan); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if len(plan.modules) > 0 {
		// Git requires one force for initialized submodules even when clean.
		// It is used only after every child file and stored object is accounted for.
		args = append(args, "--force")
	}
	args = append(args, "--", wt.Path)
	_, err = r.git(ctx, args...)
	return err
}

// removeStaleWorktree deliberately discards files only after rechecking inactivity,
// worktree identity, and the absence of child repository storage.
func (r repository) removeStaleWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool, days int) error {
	if err := r.checkWorktreeIdentity(ctx, wt, b, hasBranch); err != nil {
		return err
	}
	if hasModules, err := r.submoduleStoragePolicy(ctx, wt); err != nil {
		return err
	} else if hasModules {
		return errors.New("stale submodule worktrees require manual cleanup")
	}
	if stale, err := staleWorktree(ctx, r, wt, days, time.Now()); err != nil {
		return err
	} else if !stale {
		return errors.New("worktree activity changed; keeping local work")
	}
	if err := r.checkWorktreeIdentity(ctx, wt, b, hasBranch); err != nil {
		return err
	}
	_, err := r.git(ctx, "worktree", "remove", "--force", "--", wt.Path)
	return err
}
