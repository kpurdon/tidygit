//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRepairDetachedChild(t *testing.T) {
	if os.Getenv("TEST_REPAIR_DETACHED_HELPER") != "1" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("TEST_REPAIR_CHILD_STARTED"), []byte("started"), 0600); err != nil {
		os.Exit(3)
	}
	time.Sleep(10 * time.Second)
	if err := os.WriteFile(os.Getenv("TEST_REPAIR_CHILD_MARKER"), []byte("late mutation"), 0600); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestRepairStopsDetachedTool(t *testing.T) {
	for _, tc := range []struct {
		agent, report string
	}{{"codex", "wait-detached"}, {"claude", "wait-detached"}, {"codex", "crash-detached"}} {
		t.Run(tc.agent+"/"+tc.report, func(t *testing.T) {
			f, opts := repairFixture(t, tc.agent, tc.report, "once")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			started := filepath.Join(f.root, "child-started")
			marker := filepath.Join(f.root, "late-mutation")
			signal := filepath.Join(f.root, "signal")
			t.Setenv("TEST_REPAIR_CHILD_EXEC", executable)
			t.Setenv("TEST_REPAIR_DETACHED_HELPER", "1")
			t.Setenv("TEST_REPAIR_CHILD_STARTED", started)
			t.Setenv("TEST_REPAIR_CHILD_MARKER", marker)
			t.Setenv("TEST_REPAIR_SIGNAL", signal)
			opts.AgentTimeout = 2 * time.Second
			failure := "deadline exceeded"
			if tc.report == "crash-detached" {
				opts.AgentTimeout = 30 * time.Second
				failure = "exit status 9"
			}
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || result.Repaired || !strings.Contains(strings.Join(result.Errors, "\n"), failure) {
				t.Fatalf("expected failed repair: %+v, %v", result, err)
			}
			startedInfo, err := os.Stat(started)
			if err != nil {
				t.Fatalf("detached child did not start before timeout: %v", err)
			}
			if tc.report == "wait-detached" {
				wantSignal := "INT"
				if tc.agent == "claude" {
					wantSignal = "TERM"
				}
				if data, err := os.ReadFile(signal); err != nil || string(data) != wantSignal {
					t.Fatalf("agent did not receive graceful %s: %q, %v", wantSignal, data, err)
				}
			}
			if repairCallCount(t, "TEST_FETCH_CALLS") != 1 {
				t.Fatal("cleanup resumed after agent timeout")
			}
			// The child inherits output pipes, exercising the bounded wait after its
			// parent exits. Its write is scheduled beyond that five-second grace.
			if wait := time.Until(startedInfo.ModTime().Add(10500 * time.Millisecond)); wait > 0 {
				time.Sleep(wait)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("detached tool survived timeout and mutated files: %v", err)
			}
		})
	}
}
