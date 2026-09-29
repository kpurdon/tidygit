package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var errGHUnavailable = errors.New("GitHub CLI is unavailable")

type PR struct {
	VerifiedAlias bool `json:"-"`
	MergeCommit   *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	Branch         string `json:"headRefName"`
	State          string `json:"state"`
	HeadOID        string `json:"headRefOid"`
	HeadRepo       string `json:"-"`
	HeadRepository *struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	HeadRepositoryOwner *struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

func githubRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") || u.Hostname() == "" {
			return "", fmt.Errorf("unsupported GitHub remote %q", raw)
		}
		host, path = u.Hostname(), u.Path
	} else {
		left, right, ok := strings.Cut(raw, ":")
		if !ok || strings.ContainsAny(left, "/\\") {
			return "", fmt.Errorf("unsupported GitHub remote %q", raw)
		}
		host = left
		if _, after, ok := strings.Cut(left, "@"); ok {
			host = after
		}
		path = right
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(host+path, " \t\n?#") || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", fmt.Errorf("unsupported GitHub remote %q", raw)
	}
	return strings.ToLower(host + "/" + path), nil
}

func ghFetchPRs(ctx context.Context, r repository, branches []Branch, opts options) (map[string]PR, error) {
	result := make(map[string]PR)
	if !slices.ContainsFunc(branches, func(branch Branch) bool {
		return branch.LocalRepo != "" || branch.SourceRepo != "" && branch.SourceBranch != ""
	}) {
		return result, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("%w: %v", errGHUnavailable, err)
	}
	remotes, err := r.git(ctx, "remote")
	if err != nil {
		return nil, err
	}
	remote := "origin"
	for name := range strings.FieldsSeq(remotes) {
		if name == "upstream" {
			remote = name
			break
		}
	}
	raw, err := r.git(ctx, "remote", "get-url", remote)
	if err != nil {
		return nil, err
	}
	target, err := githubRepo(raw)
	if err != nil {
		return nil, err
	}
	result, err = fetchPRs(ctx, r.command, target, branches, opts)
	if err != nil {
		return nil, err
	}
	var pending []Branch
	for _, b := range branches {
		if _, found := result[b.Name]; !found && b.OID != "" && (b.LocalRepo != "" || b.SourceRepo != "") {
			pending = append(pending, b)
		}
	}
	resolved := make([]PR, len(pending))
	failures := make([]error, len(pending))
	workers := min(max(1, opts.Jobs), len(pending))
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := worker; i < len(pending); i += workers {
				resolved[i], failures[i] = associatedBranchPR(ctx, r, target, pending[i], opts)
			}
		})
	}
	wg.Wait()
	for i, err := range failures {
		if err != nil {
			return nil, err
		}
		if resolved[i].Number != 0 {
			result[pending[i].Name] = resolved[i]
		}
	}
	return result, nil
}

func associatedBranchPR(ctx context.Context, r repository, target string, b Branch, opts options) (PR, error) {
	source := b.LocalRepo
	if source == "" {
		source = b.SourceRepo
	}
	candidates, err := associatedPRs(ctx, r.command, target, b.OID, opts)
	if err != nil {
		return PR{}, err
	}
	hint, err := conventionalPR(ctx, r.command, target, b.Name)
	if err != nil {
		return PR{}, err
	}
	if hint.Number != 0 {
		candidates = append(candidates, hint)
	}
	var selected PR
	for _, candidate := range candidates {
		if candidate.HeadOID != b.OID || !strings.EqualFold(candidate.HeadRepo, source) {
			continue
		}
		verified, err := ghVerifyPRHistory(ctx, r, candidate, opts)
		if err != nil {
			return PR{}, err
		}
		verified.VerifiedAlias = verified.HeadOID == b.OID
		// An open PR on an associated source branch protects the local commit.
		if selected.Number == 0 || verified.State == "OPEN" || (selected.State != "OPEN" && verified.Number > selected.Number) {
			selected = verified
		}
	}
	return selected, nil
}

func fetchPRs(ctx context.Context, command func(context.Context, string, ...string) (string, error), target string, branches []Branch, opts options) (map[string]PR, error) {
	named := make([]Branch, 0, len(branches))
	for _, b := range branches {
		if b.SourceBranch == "" && b.LocalRepo == "" {
			continue
		}
		if b.LocalRepo != "" {
			b.SourceRepo = b.LocalRepo
		}
		b.SourceBranch = b.Name
		named = append(named, b)
	}
	result, err := fetchSourcePRs(ctx, command, target, named, opts)
	if err != nil {
		return nil, err
	}
	var aliases []Branch
	for _, b := range branches {
		if _, found := result[b.Name]; !found && b.SourceBranch != b.Name {
			aliases = append(aliases, b)
		}
	}
	other, err := fetchSourcePRs(ctx, command, target, aliases, opts)
	if err != nil {
		return nil, err
	}
	for _, b := range aliases {
		if pr, ok := other[b.Name]; ok {
			pr.VerifiedAlias = pr.HeadOID == b.OID && b.OID != ""
			// A tracked parent or sibling is not authoritative for a renamed
			// local branch. Keep looking for its own PR unless this alias is
			// active or identifies exactly the same commit.
			if pr.State == "OPEN" || pr.VerifiedAlias {
				result[b.Name] = pr
			}
		}
	}
	return result, nil
}

func fetchSourcePRs(ctx context.Context, command func(context.Context, string, ...string) (string, error), target string, branches []Branch, opts options) (map[string]PR, error) {
	result := make(map[string]PR)
	if !slices.ContainsFunc(branches, func(branch Branch) bool { return branch.SourceRepo != "" && branch.SourceBranch != "" }) {
		return result, nil
	}
	limit := max(1, opts.PRLimit)
	query := func(head, author string) ([]PR, error) {
		args := []string{"pr", "list", "--repo", target, "--state", "all", "--limit", strconv.Itoa(limit), "--json", "headRefName,headRefOid,headRepository,headRepositoryOwner,number,title,url,state,mergeCommit"}
		if head != "" {
			args = append(args, "--head", head)
		}
		if author != "" {
			args = append(args, "--author", author)
		}
		out, err := command(ctx, "gh", args...)
		if err != nil {
			return nil, fmt.Errorf("listing GitHub pull requests: %w", err)
		}
		var prs []PR
		if err := json.Unmarshal([]byte(out), &prs); err != nil {
			return nil, fmt.Errorf("parsing PR data: %w", err)
		}
		host, _, _ := strings.Cut(target, "/")
		for i := range prs {
			pr := &prs[i]
			if pr.HeadRepository != nil && pr.HeadRepositoryOwner != nil && pr.HeadRepository.Name != "" && pr.HeadRepositoryOwner.Login != "" {
				pr.HeadRepo = strings.ToLower(host + "/" + pr.HeadRepositoryOwner.Login + "/" + pr.HeadRepository.Name)
			}
		}
		return prs, nil
	}
	batch, err := query("", opts.PRAuthor)
	if err != nil {
		return nil, err
	}
	// Deduplicate by source branch: GitHub returns all matching fork identities in one lookup.
	targeted := make(map[string]int)
	var heads []string
	for _, branch := range branches {
		if branch.SourceRepo == "" || branch.SourceBranch == "" {
			continue
		}
		pr, found := newestPR(batch, branch)
		if !found || pr.State == "MERGED" || pr.State == "CLOSED" {
			if _, exists := targeted[branch.SourceBranch]; !exists {
				targeted[branch.SourceBranch] = len(heads)
				heads = append(heads, branch.SourceBranch)
			}
		}
	}
	results := make([][]PR, len(heads))
	failures := make([]error, len(heads))
	workers := min(max(1, opts.Jobs), len(heads))
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := worker; i < len(heads); i += workers {
				prs, err := query(heads[i], "")
				if err == nil && len(prs) >= limit {
					// A bounded lookup must not mistake a truncated history for complete evidence.
					err = fmt.Errorf("PR lookup for %q reached limit %d; increase --pr-limit", heads[i], limit)
				}
				results[i], failures[i] = prs, err
			}
		})
	}
	wg.Wait()
	// Report failures in input order, independent of concurrent completion order.
	for _, err := range failures {
		if err != nil {
			return nil, err
		}
	}
	for _, branch := range branches {
		if branch.SourceRepo == "" || branch.SourceBranch == "" {
			continue
		}
		prs := batch
		if i, exists := targeted[branch.SourceBranch]; exists {
			prs = results[i]
		}
		if pr, found := newestPR(prs, branch); found {
			result[branch.Name] = pr
		}
	}
	return result, nil
}

func newestPR(prs []PR, branch Branch) (PR, bool) {
	var selected PR
	found := false
	for _, pr := range prs {
		if pr.HeadRepo == "" || !strings.EqualFold(pr.HeadRepo, branch.SourceRepo) || pr.Branch != branch.SourceBranch {
			continue
		}
		if !found || (pr.State == "OPEN" && selected.State != "OPEN") || (pr.State == selected.State && pr.Number > selected.Number) || (pr.State != "OPEN" && selected.State != "OPEN" && pr.Number > selected.Number) {
			selected, found = pr, true
		}
	}
	return selected, found
}
