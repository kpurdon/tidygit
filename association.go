package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
)

func ghTarget(ctx context.Context, r repository) (string, error) {
	remotes, err := r.git(ctx, "remote")
	if err != nil {
		return "", err
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
		return "", err
	}
	return githubRepo(raw)
}

func ghAssociatedPRs(ctx context.Context, r repository, oid string, opts options) ([]PR, error) {
	target, err := ghTarget(ctx, r)
	if err != nil {
		return nil, err
	}
	return associatedPRs(ctx, r.command, target, oid, opts)
}

func associatedPRs(ctx context.Context, command func(context.Context, string, ...string) (string, error), target, oid string, opts options) ([]PR, error) {
	parts := strings.Split(target, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid GitHub repository %q", target)
	}
	query := `query($owner:String!,$repo:String!,$oid:GitObjectID!,$limit:Int!){repository(owner:$owner,name:$repo){object(oid:$oid){... on Commit{associatedPullRequests(first:$limit){nodes{number title url headRefName headRefOid state headRepository{name owner{login}} mergeCommit{oid}} pageInfo{hasNextPage}}}}}}`
	out, err := command(ctx, "gh", "api", "graphql", "--hostname", parts[0], "-f", "query="+query, "-f", "owner="+parts[1], "-f", "repo="+parts[2], "-f", "oid="+oid, "-F", fmt.Sprintf("limit=%d", min(100, max(1, opts.PRLimit))))
	if err != nil {
		return nil, fmt.Errorf("looking up commit pull requests: %w", err)
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			Repository *struct {
				Object *struct {
					AssociatedPullRequests struct {
						Nodes []struct {
							PR
							HeadRepository *struct {
								Name  string `json:"name"`
								Owner struct {
									Login string `json:"login"`
								} `json:"owner"`
							} `json:"headRepository"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"associatedPullRequests"`
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("parsing associated pull requests: %w", err)
	}
	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("associated pull request lookup failed: %s", response.Errors[0].Message)
	}
	if response.Data.Repository == nil {
		return nil, fmt.Errorf("associated pull request repository unavailable")
	}
	if response.Data.Repository.Object == nil {
		return nil, nil
	}
	connection := response.Data.Repository.Object.AssociatedPullRequests
	if connection.PageInfo.HasNextPage {
		return nil, fmt.Errorf("associated pull request history exceeds lookup limit")
	}
	var result []PR
	for _, node := range connection.Nodes {
		pr := node.PR
		if node.HeadRepository != nil && node.HeadRepository.Name != "" && node.HeadRepository.Owner.Login != "" {
			pr.HeadRepo = strings.ToLower(parts[0] + "/" + node.HeadRepository.Owner.Login + "/" + node.HeadRepository.Name)
		}
		result = append(result, pr)
	}
	return result, nil
}

// Recheck the complete source branch history without an author restriction so a
// later open pull request prevents cleanup based on an older terminal one.
func ghVerifyPRHistory(ctx context.Context, r repository, pr PR, opts options) (PR, error) {
	target, err := ghTarget(ctx, r)
	if err != nil {
		return PR{}, err
	}
	b := Branch{Name: pr.Branch, SourceBranch: pr.Branch, SourceRepo: pr.HeadRepo, OID: pr.HeadOID}
	found, err := fetchSourcePRs(ctx, r.command, target, []Branch{b}, opts)
	if err != nil {
		return PR{}, err
	}
	verified, ok := found[b.Name]
	if !ok {
		return PR{}, fmt.Errorf("pull request source history unavailable")
	}
	return verified, nil
}

var conventionalPRBranch = regexp.MustCompile(`^pr-?([1-9][0-9]*)$`)

// A conventional checkout name is only a lookup hint. Callers must independently
// verify the source repository, exact tip and current source branch PR history.
func conventionalPR(ctx context.Context, command func(context.Context, string, ...string) (string, error), target, name string) (PR, error) {
	match := conventionalPRBranch.FindStringSubmatch(name)
	if match == nil {
		return PR{}, nil
	}
	parts := strings.Split(target, "/")
	if len(parts) != 3 {
		return PR{}, fmt.Errorf("invalid GitHub repository %q", target)
	}
	query := `query($owner:String!,$repo:String!,$number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){number title url headRefName headRefOid state headRepository{name owner{login}} mergeCommit{oid}}}}`
	out, err := command(ctx, "gh", "api", "graphql", "--hostname", parts[0], "-f", "query="+query, "-f", "owner="+parts[1], "-f", "repo="+parts[2], "-F", "number="+match[1])
	// gh exits nonzero for GraphQL NOT_FOUND; inspect a structured response before
	// deciding whether this is simply a checkout name without a matching PR.
	if err != nil && out == "" {
		return PR{}, fmt.Errorf("looking up conventional PR checkout: %w", err)
	}
	var response struct {
		Errors []struct {
			Message string   `json:"message"`
			Type    string   `json:"type"`
			Path    []string `json:"path"`
		} `json:"errors"`
		Data struct {
			Repository *struct {
				PullRequest *struct {
					PR
					HeadRepository *struct {
						Name  string `json:"name"`
						Owner struct {
							Login string `json:"login"`
						} `json:"owner"`
					} `json:"headRepository"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if parseErr := json.Unmarshal([]byte(out), &response); parseErr != nil {
		return PR{}, fmt.Errorf("parsing conventional PR checkout: %w", parseErr)
	}
	for _, failure := range response.Errors {
		missing := failure.Type == "NOT_FOUND" && len(failure.Path) == 2 && failure.Path[0] == "repository" && failure.Path[1] == "pullRequest" && response.Data.Repository != nil && response.Data.Repository.PullRequest == nil
		if !missing {
			return PR{}, fmt.Errorf("conventional PR lookup failed: %s", failure.Message)
		}
	}
	if err != nil && len(response.Errors) == 0 {
		return PR{}, fmt.Errorf("looking up conventional PR checkout: %w", err)
	}
	if response.Data.Repository == nil {
		return PR{}, fmt.Errorf("conventional PR repository unavailable")
	}
	node := response.Data.Repository.PullRequest
	if node == nil {
		return PR{}, nil
	}
	pr := node.PR
	if node.HeadRepository != nil && node.HeadRepository.Name != "" && node.HeadRepository.Owner.Login != "" {
		pr.HeadRepo = strings.ToLower(parts[0] + "/" + node.HeadRepository.Owner.Login + "/" + node.HeadRepository.Name)
	}
	return pr, nil
}
