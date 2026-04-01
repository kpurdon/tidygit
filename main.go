package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type options struct {
	Auto    bool
	PRLimit int
	PRAuthor string
}

func defaultOptions() options {
	return options{
		PRLimit:  100,
		PRAuthor: "@me",
	}
}

func main() {
	opts := defaultOptions()

	var args []string
	for i := 0; i < len(os.Args[1:]); i++ {
		a := os.Args[1+i]
		switch a {
		case "--auto":
			opts.Auto = true
		case "--exclude-pr-filtering":
			opts.PRAuthor = ""
		case "--pr-limit":
			if i+1 >= len(os.Args[1:]) {
				fmt.Fprintf(os.Stderr, "--pr-limit requires a value\n")
				os.Exit(1)
			}
			i++
			v, err := strconv.Atoi(os.Args[1+i])
			if err != nil || v < 1 {
				fmt.Fprintf(os.Stderr, "--pr-limit must be a positive integer\n")
				os.Exit(1)
			}
			opts.PRLimit = v
		default:
			args = append(args, a)
		}
	}

	if len(args) == 0 {
		result := clean(".", true, opts)
		if len(result.Errors) > 0 {
			os.Exit(1)
		}
		return
	}

	switch args[0] {
	case "all":
		dir := "."
		if len(args) > 1 {
			dir = args[1]
		}
		if err := cleanAll(dir, opts); err != nil {
			uiErr(err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Usage: tidygit [--auto] [--pr-limit N] [--exclude-pr-filtering] [all [dir]]\n")
		os.Exit(1)
	}
}

func cleanAll(dir string, opts options) error {
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

		results = append(results, clean(repoPath, false, opts))

		// Always stop the progress spinner before next iteration,
		// even if clean() returned early without stopping it.
		uiStopProgress()
	}

	// Final screen: summary only
	uiClearScreen()
	uiSummary(results)

	return nil
}
