package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestParseArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--auto", "all", "repos", "--dry-run"},
		{"all", "repos", "--auto", "--dry-run"},
	} {
		opts, dir, all, err := parseArgs(args)
		if err != nil || !opts.Auto || !opts.DryRun || dir != "repos" || !all {
			t.Fatalf("%v: %+v %q %v %v", args, opts, dir, all, err)
		}
	}
	opts, dir, all, err := parseArgs([]string{"--force", "--timeout=2s", "--jobs", "2", "--pr-limit=5", "--exclude-pr-filtering", "all", "--", "-repos"})
	if err != nil || !opts.Force || dir != "-repos" || !all || opts.Timeout != 2*time.Second || opts.Jobs != 2 || opts.PRLimit != 5 || opts.PRAuthor != "" {
		t.Fatalf("flags: %+v %q %v %v", opts, dir, all, err)
	}
	for _, args := range [][]string{
		{"--auto", "all", "repos", "--dryrun"},
		{"all", "repos", "extra"},
		{"--pr-limit"}, {"--pr-limit", "0"}, {"--jobs=-1"}, {"--timeout=0s"}, {"--auto=false"}, {"--stale-days=-1"}, {"--stale-days=30d"},
	} {
		if _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
	}
	for _, days := range []string{"0", "7", "14", "30"} {
		opts, _, _, err := parseArgs([]string{"--stale-days", days})
		if err != nil || fmt.Sprint(opts.StaleDays) != days {
			t.Fatalf("stale threshold %q: %+v, %v", days, opts, err)
		}
	}
	if defaultOptions().StaleDays != 14 {
		t.Fatal("stale cleanup must default to 14 days")
	}
}

func TestUnknownFlagDoesNotStartCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := run(t.Context(), []string{"--auto", "all", dir, "--dryrun"}); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("expected unknown option, got %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory changed: %v %v", entries, err)
	}
}

func TestCommandProcess(t *testing.T) {
	switch os.Getenv("TIDYGIT_COMMAND_HELPER") {
	case "output":
		_, _ = os.Stdout.WriteString(`{"ok":true}`)
		_, _ = os.Stderr.WriteString("warning on stderr\n")
		os.Exit(0)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestCommandSeparatesDiagnosticsAndHonorsDeadline(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := repository{Dir: t.TempDir(), Timeout: 5 * time.Second}
	t.Setenv("TIDYGIT_COMMAND_HELPER", "output")
	out, err := r.command(t.Context(), executable, "-test.run=^TestCommandProcess$")
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("output=%q, err=%v", out, err)
	}
	t.Setenv("TIDYGIT_COMMAND_HELPER", "wait")
	r.Timeout = 50 * time.Millisecond
	_, err = r.command(t.Context(), executable, "-test.run=^TestCommandProcess$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.command(ctx, executable, "-test.run=^TestCommandProcess$")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestCancellationStopsAllAndPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cleanAll(ctx, t.TempDir(), defaultOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("all did not cancel: %v", err)
	}
	if _, err := approve(ctx, "Delete?", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("auto approval did not cancel: %v", err)
	}
	model, cmd := (confirmModel{value: true}).Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !model.(confirmModel).aborted || cmd == nil {
		t.Fatal("Ctrl+C did not abort prompt")
	}
	model, _ = (confirmModel{value: false}).Update(tea.KeyReleaseMsg{Code: 'y'})
	if model.(confirmModel).done {
		t.Fatal("key release confirmed destructive prompt")
	}
}

func TestDiscoverMissingPath(t *testing.T) {
	if _, err := discoverRepo(t.Context(), filepath.Join(t.TempDir(), "missing"), time.Second); err == nil {
		t.Fatal("missing repository accepted")
	}
}
