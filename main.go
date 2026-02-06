package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		result := clean(".", true)
		if len(result.Errors) > 0 {
			os.Exit(1)
		}
		return
	}

	switch os.Args[1] {
	case "all":
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := cleanAll(dir); err != nil {
			uiErr(err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Usage: tidygit [all [dir]]\n")
		os.Exit(1)
	}
}

func cleanAll(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", dir, err)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", absDir, err)
	}

	// Collect repo paths and names upfront.
	var repoPaths []string
	var repoNames []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoPath := filepath.Join(absDir, entry.Name())
		gitDir := filepath.Join(repoPath, ".git")
		info, err := os.Stat(gitDir)
		if err != nil || !info.IsDir() {
			continue
		}
		repoPaths = append(repoPaths, repoPath)
		repoNames = append(repoNames, entry.Name())
	}

	if len(repoPaths) == 0 {
		fmt.Println("No git repositories found.")
		return nil
	}

	var results []repoResult

	for i, repoPath := range repoPaths {
		uiClearScreen()
		uiBrand()
		uiProgressSpinner(i+1, len(repoPaths), repoNames[i])

		results = append(results, clean(repoPath, false))

		// Always stop the progress spinner before next iteration,
		// even if clean() returned early without stopping it.
		uiStopProgress()
	}

	// Final screen: summary only
	uiClearScreen()
	uiSummary(results)

	return nil
}
