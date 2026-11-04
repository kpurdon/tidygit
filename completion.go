package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// integratedBranches only admits discovered tips with no matching PR after a
// successful PR lookup. Ref changes during the query cannot bless a new tip.
func integratedBranches(ctx context.Context, r repository, base string, branches []Branch, prs map[string]PR) (map[string]bool, error) {
	pending := map[string]string{}
	for _, b := range branches {
		if _, found := prs[b.Name]; !found && b.LocalRepo != "" {
			pending[b.Name] = b.OID
		}
	}
	result := map[string]bool{}
	if len(pending) == 0 {
		return result, nil
	}
	grafts := os.Getenv("GIT_GRAFT_FILE")
	if grafts == "" {
		grafts = filepath.Join(r.CommonDir, "info", "grafts")
	} else if !filepath.IsAbs(grafts) {
		grafts = filepath.Join(r.Dir, grafts)
	}
	// --no-replace-objects does not disable graft files. Rewritten ancestry
	// cannot establish that the remote preserves a branch's original history.
	if info, err := os.Stat(grafts); err == nil && info.Size() > 0 {
		return result, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out, err := r.git(ctx, "--no-replace-objects", "for-each-ref", "--merged=refs/remotes/origin/"+base, "--format=%(refname:strip=2)%00%(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		name, oid, ok := strings.Cut(line, "\x00")
		if ok && oid != "" && pending[name] == oid {
			result[name] = true
		}
	}
	return result, nil
}

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
