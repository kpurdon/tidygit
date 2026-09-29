package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Detached snapshots need independent evidence: ancestry, an exact terminal PR
// head, or a squash-equivalent change whose complete history will be archived.
func detachedCompletion(ctx context.Context, r repository, wt Worktree, base string, opts options) (pr PR, complete, archive bool, err error) {
	raw, err := r.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return pr, false, false, err
	}
	source, err := githubRepo(raw)
	if err != nil {
		return pr, false, false, err
	}
	associated, err := ghAssociatedPRs(ctx, r, wt.OID, opts)
	if err != nil {
		return pr, false, false, err
	}
	ancestor, err := r.isAncestor(ctx, wt.OID, "refs/remotes/origin/"+base)
	if err != nil {
		return pr, false, false, err
	}
	var evidence PR
	for _, candidate := range associated {
		if candidate.HeadRepo != source {
			continue
		}
		verified, verifyErr := ghVerifyPRHistory(ctx, r, candidate, opts)
		if verifyErr != nil {
			return pr, false, false, verifyErr
		}
		if verified.State == "OPEN" {
			return verified, false, false, nil
		}
		if ancestor {
			continue
		}
		if verified.Number != candidate.Number {
			continue
		}
		pr = verified
		if (pr.State == "MERGED" || pr.State == "CLOSED") && pr.HeadOID == wt.OID {
			complete = true
			evidence = pr
		}
		if !complete && pr.State == "MERGED" && pr.MergeCommit != nil && pr.MergeCommit.OID != "" {
			// A read-only preview may see a PR merged since the last local fetch.
			// Missing optional evidence cannot establish squash completion.
			_, lookupErr := r.git(ctx, "rev-parse", "--verify", "--quiet", pr.MergeCommit.OID+"^{commit}")
			if lookupErr != nil {
				if exit, ok := errors.AsType[*exec.ExitError](lookupErr); ok && exit.ExitCode() == 1 {
					continue
				}
				return pr, false, false, lookupErr
			}
			integrated, checkErr := r.isAncestor(ctx, pr.MergeCommit.OID, "refs/remotes/origin/"+base)
			if checkErr != nil {
				return pr, false, false, checkErr
			}
			if integrated {
				equal, checkErr := r.sameSquashPatch(ctx, wt.OID, pr.MergeCommit.OID)
				if checkErr != nil {
					return pr, false, false, checkErr
				}
				if equal && !complete {
					complete, archive = true, true
					evidence = pr
				}
			}
		}
	}
	if ancestor {
		return PR{}, true, false, nil
	}
	if complete {
		return evidence, complete, archive, nil
	}
	return pr, false, false, nil
}

func (r repository) isAncestor(ctx context.Context, tip, base string) (bool, error) {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", tip, base)
	if err == nil {
		return true, nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func (r repository) sameSquashPatch(ctx context.Context, tip, merged string) (bool, error) {
	parents, err := r.git(ctx, "rev-list", "--parents", "-n", "1", merged)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(parents)
	if len(fields) != 2 { // Only a single-parent squash has an unambiguous aggregate patch.
		return false, nil
	}
	base, err := r.git(ctx, "merge-base", tip, fields[1])
	if err != nil {
		return false, err
	}
	local, err := r.patchID(ctx, strings.TrimSpace(base), tip)
	if err != nil {
		return false, err
	}
	integrated, err := r.patchID(ctx, fields[1], merged)
	return local != "" && local == integrated, err
}

func (r repository) patchID(ctx context.Context, base, tip string) (string, error) {
	diff, err := r.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", base, tip, "--")
	if err != nil || diff == "" {
		return "", err
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	cmd.Dir = r.Dir
	cmd.Stdin = strings.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("comparing squash changes: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (r repository) keptBranches(ctx context.Context) (map[string]bool, error) {
	out, err := r.git(ctx, "config", "--null", "--get-all", "tidygit.keep")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	kept := map[string]bool{}
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" {
			kept[name] = true
		}
	}
	return kept, nil
}
