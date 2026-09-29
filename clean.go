package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type repoResult struct {
	Name, DefaultBranch                                                  string
	WorktreesTotal, WorktreesRemoved, WorktreesSkipped, WorktreesPlanned int
	BranchesTotal, BranchesDeleted, BranchesSkipped, BranchesPlanned     int
	PRsFound                                                             int
	DryRun                                                               bool
	Duration                                                             time.Duration
	Canceled                                                             bool
	Errors                                                               []string
}

func (r *repoResult) addErr(msg string, err error) {
	text := fmt.Sprintf("%s: %v", msg, err)
	r.Errors = append(r.Errors, text)
	uiErr(text)
}

func clean(ctx context.Context, r repository, opts options) (result repoResult, returnedErr error) {
	started := time.Now()
	defer func() {
		result.Duration = time.Since(started)
		result.Canceled = errors.Is(returnedErr, context.Canceled) || errors.Is(returnedErr, ErrUserAborted)
	}()
	result = repoResult{Name: filepath.Base(r.Dir), DryRun: opts.DryRun}
	fail := func(msg string, err error) (repoResult, error) { result.addErr(msg, err); return result, ctx.Err() }
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if state, err := r.inProgress(ctx); err != nil {
		return fail("checking repository state", err)
	} else if state != "" {
		return fail("checking repository state", fmt.Errorf("%s in progress; finish or abort it before cleanup", state))
	}
	uiSection(result.Name)
	detecting := uiSpinner("Detecting default branch")
	defaultBranch, err := r.defaultBranch(ctx)
	detecting()
	if err != nil {
		return fail("detecting default branch", err)
	}
	result.DefaultBranch = defaultBranch
	uiDim("Default branch: " + defaultBranch)
	if opts.DryRun {
		uiWarn("Dry run: repositories will not be changed")
	}
	if !opts.DryRun {
		done := uiSpinner("Fetching")
		_, err = r.git(ctx, "fetch", "--all", "--prune")
		done()
		if err != nil {
			return fail("fetching", err)
		}
		uiOK("Fetched (pruned remotes)")
		status, err := r.status(ctx, r.Dir, false)
		if err != nil {
			return fail("checking changes", err)
		}
		if status != "" {
			uiWarn("Current worktree has changes; keeping its branch and files untouched")
		} else {
			worktrees, err := r.worktrees(ctx)
			if err != nil {
				return fail("checking worktrees", err)
			}
			occupied := false
			for _, wt := range worktrees {
				if wt.Branch == defaultBranch && canonicalPath(wt.Path) != r.Dir {
					occupied = true
				}
			}
			if occupied {
				uiWarn("Default branch is in another worktree; leaving the current checkout untouched")
			} else {
				if _, err = r.git(ctx, "switch", "--no-overwrite-ignore", defaultBranch); err != nil {
					return fail("switching to default branch", err)
				}
				uiOK("Switched to " + defaultBranch)
				args := []string{"merge", "--ff-only", "--no-overwrite-ignore", "refs/remotes/origin/" + defaultBranch}
				update := true
				if opts.Rebase {
					status, err := r.status(ctx, r.Dir, true)
					if err != nil {
						return fail("checking ignored files before rebase", err)
					}
					if status != "" {
						uiWarn("Local files are present; skipping rebase to preserve them")
						update = false
					}
					args = []string{"rebase", "refs/remotes/origin/" + defaultBranch}
				}
				if update {
					if _, err = r.git(ctx, args...); err != nil {
						return fail("updating default branch", err)
					}
					uiOK("Updated " + defaultBranch)
				}
			}
		}
		if _, err = r.git(ctx, "worktree", "prune"); err != nil {
			return fail("pruning worktrees", err)
		}
	}
	branches, err := r.branches(ctx)
	if err != nil {
		return fail("listing branches", err)
	}
	worktrees, err := r.worktrees(ctx)
	if err != nil {
		return fail("listing worktrees", err)
	}
	activeOperation, err := r.worktreeOperation(ctx, worktrees)
	if err != nil {
		return fail("checking linked worktree operations", err)
	}
	branchByName := map[string]Branch{}
	var candidates []Branch
	for _, b := range branches {
		if b.Name != defaultBranch {
			branchByName[b.Name] = b
			candidates = append(candidates, b)
		}
	}
	result.BranchesTotal = len(candidates)
	if len(worktrees) > 0 {
		result.WorktreesTotal = len(worktrees) - 1
	}
	protectedBranches := map[string]bool{}
	for i, wt := range worktrees {
		if i == 0 || r.protects(wt.Path) || wt.Locked || wt.Prunable || wt.Branch == defaultBranch {
			protectedBranches[wt.Branch] = true
		}
	}
	var lookupCandidates []Branch
	for _, b := range candidates {
		if !protectedBranches[b.Name] {
			lookupCandidates = append(lookupCandidates, b)
		}
	}
	done := uiSpinner("Checking PRs")
	prs, err := ghFetchPRs(ctx, r, lookupCandidates, opts)
	done()
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if opts.Auto || !errors.Is(err, errGHUnavailable) {
			return fail("fetching PRs", err)
		}
		uiWarn("GitHub CLI is unavailable; PR status cannot be verified")
		prs = map[string]PR{}
	}
	result.PRsFound = len(prs)
	handled := map[string]bool{}
	keepBranch := func(name string) {
		if _, ok := branchByName[name]; ok && !handled[name] {
			handled[name] = true
			result.BranchesSkipped++
		}
	}
	for index, wt := range worktrees {
		if index == 0 {
			keepBranch(wt.Branch)
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		uiWorktree(r.Dir, wt)
		if pr, ok := prs[wt.Branch]; ok {
			uiPR(pr)
		}
		reason := ""
		switch {
		case r.protects(wt.Path):
			reason = "current worktree"
		case wt.Branch == defaultBranch:
			reason = "default branch"
		case wt.Locked:
			reason = "locked worktree"
		case wt.Prunable:
			reason = "unavailable worktree"
		}
		b, hasBranch := branchByName[wt.Branch]
		if reason == "" && hasBranch && activeOperation != "" {
			reason = activeOperation
		}
		if reason == "" && (opts.Auto || opts.DryRun) {
			if !hasBranch {
				reason = "detached worktree has no branch to verify"
			} else {
				reason = cleanupKeepReason(prs[wt.Branch], b)
			}
		}
		if reason == "" {
			status, err := r.status(ctx, wt.Path, true)
			if err != nil {
				result.addErr("checking worktree", err)
				reason = "unable to verify files"
			} else if status != "" {
				reason = "modified, untracked, or ignored files"
			}
		}
		if reason == "" {
			linked := r
			linked.Dir = wt.Path
			if state, err := linked.inProgress(ctx); err != nil {
				result.addErr("checking worktree state", err)
				reason = "unable to verify repository state"
			} else if state != "" {
				reason = state + " in progress"
			}
		}
		if reason != "" {
			uiKeep(reason)
			result.WorktreesSkipped++
			keepBranch(wt.Branch)
			continue
		}
		if hasBranch && !opts.Auto && !opts.DryRun {
			uiLocalCommits(ctx, r, defaultBranch, b)
		}
		if opts.DryRun {
			uiOK("Would remove worktree " + wt.Path)
			result.WorktreesPlanned++
			if hasBranch {
				uiOK("Would delete branch " + b.Name + " at " + b.OID)
				result.BranchesPlanned++
				handled[b.Name] = true
			}
			continue
		}
		title := "Remove this clean worktree?"
		if hasBranch {
			title = "Remove this clean worktree and delete its branch?"
		}
		yes, err := approve(ctx, title, opts.Auto)
		if err != nil {
			return result, err
		}
		if !yes {
			uiKeep("declined")
			result.WorktreesSkipped++
			keepBranch(wt.Branch)
			continue
		}
		if err := r.removeWorktree(ctx, wt, b, hasBranch); err != nil {
			result.addErr("removing worktree", err)
			result.WorktreesSkipped++
			keepBranch(wt.Branch)
			continue
		}
		uiOK("Removed worktree")
		result.WorktreesRemoved++
		if hasBranch {
			handled[b.Name] = true
			deleted, err := r.deleteBranch(ctx, b)
			if err != nil {
				result.addErr("deleting branch "+b.Name, err)
			}
			if deleted {
				uiOK("Deleted branch " + b.Name)
				result.BranchesDeleted++
			} else {
				result.BranchesSkipped++
			}
		}
	}
	checkedOut := map[string]bool{}
	remaining, err := r.worktrees(ctx)
	if err != nil {
		return fail("checking remaining worktrees", err)
	}
	for _, wt := range remaining {
		checkedOut[wt.Branch] = true
	}
	for _, b := range candidates {
		if handled[b.Name] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		uiBranch(b)
		pr, hasPR := prs[b.Name]
		if hasPR {
			uiPR(pr)
		}
		reason := ""
		if checkedOut[b.Name] {
			reason = "checked out in a retained worktree"
		} else if activeOperation != "" {
			reason = activeOperation
		} else if opts.Auto || opts.DryRun {
			reason = cleanupKeepReason(pr, b)
		}
		if reason != "" {
			uiKeep(reason)
			result.BranchesSkipped++
			continue
		}
		if !opts.Auto && !opts.DryRun {
			uiLocalCommits(ctx, r, defaultBranch, b)
		}
		if opts.DryRun {
			uiOK("Would delete branch " + b.Name + " at " + b.OID)
			result.BranchesPlanned++
			continue
		}
		yes, err := approve(ctx, "Delete this branch?", opts.Auto)
		if err != nil {
			return result, err
		}
		if !yes {
			uiKeep("declined")
			result.BranchesSkipped++
			continue
		}
		deleted, err := r.deleteBranch(ctx, b)
		if err != nil {
			result.addErr("deleting branch "+b.Name, err)
		}
		if deleted {
			uiOK("Deleted branch " + b.Name)
			result.BranchesDeleted++
		} else {
			result.BranchesSkipped++
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func approve(ctx context.Context, title string, auto bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if auto {
		return true, nil
	}
	return confirm(ctx, title, false)
}
func uiLocalCommits(ctx context.Context, r repository, base string, b Branch) {
	out, err := r.git(ctx, "log", "--oneline", "-5", "refs/remotes/origin/"+base+".."+b.OID, "--")
	if err == nil && strings.TrimSpace(out) != "" {
		uiWarn("Commits not reachable from origin/" + base + " (may have been squash/rebase merged):")
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			uiDim(displayText(line))
		}
	}
}
func (r repository) removeWorktree(ctx context.Context, wt Worktree, b Branch, hasBranch bool) error {
	fresh, err := r.worktrees(ctx)
	if err != nil {
		return err
	}
	found := false
	for index, current := range fresh {
		if canonicalPath(current.Path) != canonicalPath(wt.Path) {
			continue
		}
		if index == 0 || r.protects(current.Path) || current.Locked || current.Prunable || current.OID != wt.OID || current.Branch != wt.Branch {
			return errors.New("worktree changed or is protected; keeping it")
		}
		found = true
	}
	if !found {
		return errors.New("worktree disappeared since discovery")
	}
	linked := r
	linked.Dir = wt.Path
	if state, err := linked.inProgress(ctx); err != nil {
		return err
	} else if state != "" {
		return fmt.Errorf("%s in progress; keeping worktree", state)
	}
	if hasBranch {
		if err := r.checkBranch(ctx, b); err != nil {
			return err
		}
	}
	status, err := r.status(ctx, wt.Path, true)
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("worktree contains modified, untracked, or ignored files; keeping it")
	}
	_, err = r.git(ctx, "worktree", "remove", "--", wt.Path)
	return err
}

func cleanupKeepReason(pr PR, b Branch) string {
	switch pr.State {
	case "":
		return "no matching pull request"
	case "OPEN":
		return "pull request is still open"
	case "MERGED", "CLOSED":
		if b.SourceRepo == "" || pr.HeadRepo == "" || pr.HeadOID == "" {
			return "PR repository or commit could not be verified"
		}
		if pr.HeadRepo != b.SourceRepo || pr.Branch != b.SourceBranch {
			return "PR belongs to a different repository or branch"
		}
		if pr.HeadOID != b.OID {
			return "local tip differs from the PR; keeping local work"
		}
		return ""
	default:
		return "pull request state could not be verified"
	}
}
