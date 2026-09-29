package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

var version = "dev"

type options struct {
	Auto, DryRun, Rebase, Force, Help, Version bool
	PRLimit                                    int
	PRAuthor                                   string
	Timeout                                    time.Duration
	Jobs                                       int
	StaleDays                                  int
}

func defaultOptions() options {
	return options{PRLimit: 100, PRAuthor: "@me", Timeout: 30 * time.Second, Jobs: 4, StaleDays: 30}
}

const usage = `Usage: tidygit [options] [all [dir]]

Tidy the current repository or immediate repositories in a directory.

  --auto                  Clean up completed or archived stale work; never prompt
  --dry-run               Show proposed cleanup without changing repositories
  --force                 Recheck unchanged repositories; keep all deletion safeguards
  --stale-days N          Archive inactive work after N days (default 30; 0 disables)
  --pr-limit N            PR lookup limit (default 100)
  --exclude-pr-filtering  Include all authors in the initial PR lookup
  --rebase                Rebase the default branch instead of fast-forward only
  --timeout DURATION      Timeout per Git/GitHub command (default 30s)
  --jobs N                Concurrent discovery/PR lookups (default 4)
  --version               Print version
  -h, --help              Show this help
`

func parseArgs(args []string) (options, string, bool, error) {
	opts := defaultOptions()
	var positional []string
	flags := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if flags && arg == "--" {
			flags = false
			continue
		}
		if !flags || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--pr-limit", "--timeout", "--jobs", "--stale-days":
			if !hasValue {
				i++
				if i == len(args) {
					return opts, "", false, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if name == "--timeout" {
				d, err := time.ParseDuration(value)
				if err != nil || d <= 0 {
					return opts, "", false, errors.New("--timeout must be a positive duration")
				}
				opts.Timeout = d
			} else {
				n, err := strconv.Atoi(value)
				if name == "--stale-days" {
					if err != nil || n < 0 {
						return opts, "", false, errors.New("--stale-days must be a nonnegative integer")
					}
					opts.StaleDays = n
					continue
				}
				if err != nil || n < 1 {
					return opts, "", false, fmt.Errorf("%s must be a positive integer", name)
				}
				if name == "--jobs" {
					opts.Jobs = n
				} else {
					opts.PRLimit = n
				}
			}
		case "--auto", "--dry-run", "--rebase", "--force", "--exclude-pr-filtering", "--version", "--help", "-h":
			if hasValue {
				return opts, "", false, fmt.Errorf("%s does not accept a value", name)
			}
			switch name {
			case "--auto":
				opts.Auto = true
			case "--dry-run":
				opts.DryRun = true
			case "--force":
				opts.Force = true
			case "--rebase":
				opts.Rebase = true
			case "--exclude-pr-filtering":
				opts.PRAuthor = ""
			case "--version":
				opts.Version = true
			default:
				opts.Help = true
			}
		default:
			return opts, "", false, fmt.Errorf("unknown option %s", name)
		}
	}
	if len(positional) == 0 {
		return opts, ".", false, nil
	}
	if positional[0] != "all" || len(positional) > 2 {
		return opts, "", false, errors.New("expected all [dir]; see --help")
	}
	dir := "."
	if len(positional) == 2 {
		dir = positional[1]
	}
	return opts, dir, true, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrUserAborted) {
			uiErr("Canceled")
			os.Exit(130)
		}
		uiErr(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	opts, dir, all, err := parseArgs(args)
	if err != nil {
		return err
	}
	if opts.Help {
		fmt.Print(usage)
		return nil
	}
	if opts.Version {
		v := version
		if v == "dev" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
		}
		fmt.Println("tidygit " + v)
		return nil
	}
	if !opts.Auto && !opts.DryRun && !interactiveTerminal() {
		return errors.New("interactive cleanup requires a terminal; use --auto or --dry-run")
	}
	if all {
		return cleanAll(ctx, dir, opts)
	}
	repo, err := discoverRepo(ctx, dir, opts.Timeout)
	if err != nil {
		return err
	}
	uiBrand()
	uiSection(filepath.Base(repo.Dir))
	result, err := clean(ctx, repo, opts)
	uiSummary([]repoResult{result})
	if err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%s: cleanup failed (%d error(s))", result.Name, len(result.Errors))
	}
	return nil
}

func cleanAll(ctx context.Context, dir string, opts options) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return fmt.Errorf("reading directory: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		path := filepath.Join(absDir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			paths = append(paths, path)
		}
	}
	type discovered struct {
		repo repository
		err  error
	}
	found := make([]discovered, len(paths))
	var wg sync.WaitGroup
	jobs := make(chan struct{}, min(opts.Jobs, len(paths)))
	for i, path := range paths {
		wg.Go(func() {
			select {
			case jobs <- struct{}{}:
				defer func() { <-jobs }()
			case <-ctx.Done():
				found[i].err = ctx.Err()
				return
			}
			found[i].repo, found[i].err = discoverRepo(ctx, path, opts.Timeout)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	caller, _ := discoverRepo(ctx, ".", opts.Timeout)
	var repos []repository
	var failures []error
	var results []repoResult
	seen := map[string]int{}
	for i, item := range found {
		if item.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", paths[i], item.err))
			results = append(results, repoResult{Name: filepath.Base(paths[i]), DryRun: opts.DryRun, Errors: []string{item.err.Error()}})
			continue
		}
		if caller.CommonDir == item.repo.CommonDir {
			item.repo.ProtectedPath = caller.Dir
		}
		if index, ok := seen[item.repo.CommonDir]; ok {
			if filepath.Dir(item.repo.CommonDir) == item.repo.Dir {
				repos[index] = item.repo
			}
			continue
		}
		seen[item.repo.CommonDir] = len(repos)
		repos = append(repos, item.repo)
	}
	if len(repos) == 0 && len(failures) == 0 {
		fmt.Println("No git repositories found.")
		return nil
	}
	uiBrand()
	for i, repo := range repos {
		if err := ctx.Err(); err != nil {
			uiSummary(results)
			return err
		}
		uiProgress(i+1, len(repos), filepath.Base(repo.Dir))
		result, err := clean(ctx, repo, opts)
		results = append(results, result)
		if err != nil {
			uiSummary(results)
			return err
		}
		if len(result.Errors) > 0 {
			failures = append(failures, fmt.Errorf("%s: %d error(s)", result.Name, len(result.Errors)))
		}
	}
	uiSummary(results)
	return errors.Join(failures...)
}
