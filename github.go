package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

type PR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Branch string `json:"headRefName"`
}

// ghFetchOpenPRs returns a map of branch name to PR info.
// Returns an empty map if gh is not installed or not authenticated.
func ghFetchOpenPRs() (map[string]PR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return map[string]PR{}, nil
	}

	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return map[string]PR{}, nil
	}

	out, err := exec.Command(
		"gh", "pr", "list",
		"--state", "open",
		"--json", "headRefName,number,title,url",
	).CombinedOutput()
	if err != nil {
		return map[string]PR{}, nil
	}

	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parsing PR data: %w", err)
	}

	result := make(map[string]PR, len(prs))
	for _, pr := range prs {
		result[pr.Branch] = pr
	}
	return result, nil
}
