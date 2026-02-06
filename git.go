package main

import (
	"fmt"
	"os/exec"
	"strings"
)

type Worktree struct {
	Path   string
	Branch string
}

func gitDefaultBranch() (string, error) {
	out, err := exec.Command("git", "remote", "show", "origin").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("getting default branch: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HEAD branch:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:")), nil
		}
	}
	return "", fmt.Errorf("getting default branch: HEAD branch not found in remote output")
}

func gitHasUncommittedChanges() bool {
	err := exec.Command("git", "diff-index", "--quiet", "HEAD", "--").Run()
	return err != nil
}

func gitResetHard() error {
	out, err := exec.Command("git", "reset", "--hard", "HEAD").CombinedOutput()
	if err != nil {
		return fmt.Errorf("resetting HEAD: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitSwitch(branch string) error {
	out, err := exec.Command("git", "switch", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("switching to %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitFetchAll() error {
	out, err := exec.Command("git", "fetch", "--all", "--prune").CombinedOutput()
	if err != nil {
		return fmt.Errorf("fetching: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitPull(branch string) error {
	out, err := exec.Command("git", "pull", "--rebase", "origin", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pulling with rebase: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitPruneWorktrees() error {
	out, err := exec.Command("git", "worktree", "prune").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pruning worktrees: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitListWorktrees() ([]Worktree, error) {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var worktrees []Worktree
	var current Worktree
	first := true

	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if first {
				// Skip the main worktree (first entry)
				first = false
				current = Worktree{}
				continue
			}
			current = Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch "):
			if current.Path != "" {
				current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		case line == "":
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = Worktree{}
			}
		}
	}
	// Handle last entry if no trailing newline
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	return worktrees, nil
}

func gitListBranches(exclude string) ([]string, error) {
	out, err := exec.Command("git", "branch", "--format=%(refname:short)").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing branches: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != exclude {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

func gitRemoveWorktree(path string) error {
	out, err := exec.Command("git", "worktree", "remove", path, "--force").CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing worktree %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitDeleteBranch(name string) error {
	out, err := exec.Command("git", "branch", "-D", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("deleting branch %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}
