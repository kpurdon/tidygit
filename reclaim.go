package main

import (
	"context"
	"errors"
	"strings"
)

// disposableWorktree requires clean source and recoverable submodule history.
// Ignored files in a verified-completed checkout are deliberately disposable.
func (r repository) disposableWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool) (submoduleRecovery, error) {
	if err := r.checkWorktree(ctx, wt, b, hasBranch); err != nil {
		return submoduleRecovery{}, err
	}
	if err := r.checkVisibleIndex(ctx, wt.Path); err != nil {
		return submoduleRecovery{}, err
	}
	return r.disposableSubmodulePlan(ctx, wt)
}

func (r repository) checkVisibleIndex(ctx context.Context, path string) error {
	out, err := r.git(ctx, "-C", path, "ls-files", "-v", "-z")
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
	// Recheck branch identity, local changes, and operation ownership after the
	// remote submodule proof, which may have taken time to complete.
	if err := r.checkWorktree(ctx, wt, b, hasBranch); err != nil {
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
