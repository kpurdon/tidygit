package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/huh"
)

type repoResult struct {
	Name             string
	DefaultBranch    string
	WorktreesTotal   int
	WorktreesRemoved int
	WorktreesSkipped int
	BranchesTotal    int
	BranchesDeleted  int
	BranchesSkipped  int
	PRsOpen          int
	Errors           []string
}

func (r *repoResult) addErr(msg string, err error) {
	r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", msg, err))
	uiErr(fmt.Sprintf("%s: %v", msg, err))
}

func clean(dir string, showBrand bool) repoResult {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return repoResult{Errors: []string{fmt.Sprintf("resolving path: %v", err)}}
	}

	if err := os.Chdir(absDir); err != nil {
		return repoResult{Errors: []string{fmt.Sprintf("changing to directory %s: %v", absDir, err)}}
	}

	repoName := filepath.Base(absDir)
	result := repoResult{Name: repoName}

	// Detect default branch
	defaultBranch, err := gitDefaultBranch()
	if err != nil {
		result.addErr("detecting default branch", err)
	} else {
		result.DefaultBranch = defaultBranch
	}

	if showBrand {
		uiBrand()
	}
	if defaultBranch != "" {
		uiSection(fmt.Sprintf("%s (%s)", repoName, defaultBranch))
	} else {
		uiSection(repoName)
	}

	// Check for uncommitted changes
	if gitHasUncommittedChanges() {
		uiWarn("Uncommitted changes detected")
		uiStopProgress()

		var confirm bool
		err := huh.NewConfirm().
			Title("Reset HEAD and discard all changes?").
			Affirmative("Yes").
			Negative("No").
			Value(&confirm).
			Run()
		if err != nil {
			result.addErr("prompting for reset", err)
		} else if !confirm {
			uiSkipped()
		} else if err := gitResetHard(); err != nil {
			result.addErr("resetting HEAD", err)
		} else {
			uiOK("Reset to HEAD")
		}
	}

	// Switch to default branch
	onDefaultBranch := false
	if defaultBranch != "" {
		if err := gitSwitch(defaultBranch); err != nil {
			result.addErr("switching to "+defaultBranch, err)
		} else {
			uiOK("Switched to " + defaultBranch)
			onDefaultBranch = true
		}
	}

	// Fetch all
	done := uiSpinner("Fetching")
	err = gitFetchAll()
	done()
	if err != nil {
		result.addErr("fetching", err)
	} else {
		uiOK("Fetched (pruned remotes)")
	}

	// Pull with rebase (only if on default branch)
	if onDefaultBranch {
		if err := gitPull(defaultBranch); err != nil {
			result.addErr("pulling "+defaultBranch, err)
		} else {
			uiOK("Pulled " + defaultBranch + " (rebase)")
		}
	}

	// Fetch open PRs
	done = uiSpinner("Checking PRs")
	prs, err := ghFetchOpenPRs()
	done()
	if err != nil {
		result.addErr("fetching PRs", err)
		prs = map[string]PR{}
	} else {
		result.PRsOpen = len(prs)
		if len(prs) > 0 {
			uiOK(fmt.Sprintf("Found %d open PR(s)", len(prs)))
		}
	}

	// List branches early so we can detect worktree+branch overlap
	excludeBranch := defaultBranch
	if excludeBranch == "" {
		excludeBranch = "__none__"
	}
	branches, err := gitListBranches(excludeBranch)
	if err != nil {
		result.addErr("listing branches", err)
	}

	branchSet := make(map[string]struct{}, len(branches))
	for _, b := range branches {
		branchSet[b] = struct{}{}
	}

	deletedBranches := make(map[string]struct{})

	// Prune worktrees
	if err := gitPruneWorktrees(); err != nil {
		result.addErr("pruning worktrees", err)
	}

	// List worktrees
	worktrees, err := gitListWorktrees()
	if err != nil {
		result.addErr("listing worktrees", err)
	} else {
		result.WorktreesTotal = len(worktrees)
		if len(worktrees) > 0 {
			uiStopProgress()
			uiSection(fmt.Sprintf("Worktrees (%d)", len(worktrees)))

			for _, wt := range worktrees {
				_, branchExists := branchSet[wt.Branch]

				if branchExists {
					uiItem(fmt.Sprintf("%s (branch: %s)", wt.Path, wt.Branch))
				} else {
					uiItem(wt.Path)
				}

				if pr, ok := prs[wt.Branch]; ok {
					uiPR(pr)
				}

				title := "Remove worktree?"
				if branchExists {
					title = "Remove worktree and delete branch?"
				}

				var confirm bool
				err := huh.NewConfirm().
					Title(title).
					Affirmative("Yes").
					Negative("No").
					Value(&confirm).
					Run()
				if err != nil {
					result.addErr("prompting for worktree removal", err)
					continue
				}

				if confirm {
					if err := gitRemoveWorktree(wt.Path); err != nil {
						result.addErr("removing worktree "+wt.Path, err)
					} else {
						uiOK("Removed worktree")
						result.WorktreesRemoved++
					}

					if branchExists {
						if err := gitDeleteBranch(wt.Branch); err != nil {
							result.addErr("deleting branch "+wt.Branch, err)
						} else {
							uiOK("Deleted branch " + wt.Branch)
							deletedBranches[wt.Branch] = struct{}{}
							result.BranchesDeleted++
						}
					}
				} else {
					uiSkipped()
					result.WorktreesSkipped++
				}
				fmt.Println()
			}
		} else {
			uiDim("No worktrees to clean up")
		}
	}

	// Filter out branches already deleted during worktree cleanup
	var remainingBranches []string
	for _, b := range branches {
		if _, deleted := deletedBranches[b]; !deleted {
			remainingBranches = append(remainingBranches, b)
		}
	}

	result.BranchesTotal = len(remainingBranches)
	if len(remainingBranches) > 0 {
		uiStopProgress()
		uiSection(fmt.Sprintf("Branches (%d)", len(remainingBranches)))

		for _, branch := range remainingBranches {
			uiItem(branch)

			if pr, ok := prs[branch]; ok {
				uiPR(pr)
			}

			var confirm bool
			err := huh.NewConfirm().
				Title("Delete branch?").
				Affirmative("Yes").
				Negative("No").
				Value(&confirm).
				Run()
			if err != nil {
				result.addErr("prompting for branch deletion", err)
				continue
			}

			if confirm {
				if err := gitDeleteBranch(branch); err != nil {
					result.addErr("deleting branch "+branch, err)
				} else {
					uiOK("Deleted")
					result.BranchesDeleted++
				}
			} else {
				uiSkipped()
				result.BranchesSkipped++
			}
			fmt.Println()
		}
	} else {
		uiDim("No branches to clean up")
	}

	uiStopProgress()
	uiDone()

	return result
}
