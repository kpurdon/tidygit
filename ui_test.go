package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestSummaryReportsActualPlannedAndPartialProgress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result repoResult
		want   []string
		absent string
	}{
		{"unchanged", repoResult{Name: "repo", Unchanged: true}, []string{"unchanged"}, "0 removed"},
		{"actual", repoResult{Name: "repo", WorktreesRemoved: 1, BranchesDeleted: 2, Duration: time.Second}, []string{"done", "1 removed", "2 deleted", "1s"}, "planned"},
		{"preview", repoResult{Name: "repo", DryRun: true, WorktreesPlanned: 1, BranchesPlanned: 2}, []string{"dry run", "1 planned", "2 planned"}, "deleted"},
		{"canceled", repoResult{Name: "repo", Canceled: true, BranchesDeleted: 1}, []string{"canceled", "1 deleted", "partial progress"}, "done"},
		{"failed", repoResult{Name: "repo", Errors: []string{"git failed\nreason"}}, []string{"failed", "git failed", "reason"}, `git failed\nreason`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			previous := os.Stdout
			os.Stdout = f
			defer func() { os.Stdout = previous }()
			uiSummary([]repoResult{tc.result})
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			if strings.Contains(got, "\x1b") {
				t.Errorf("redirected output includes escape sequences: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			if strings.Contains(got, tc.absent) {
				t.Errorf("unexpected %q in %q", tc.absent, got)
			}
		})
	}
}

func TestDisplaySafetyAndWrapping(t *testing.T) {
	for _, value := range []string{"branch\x1b[2J", "branch\rforged", "branch\nforged"} {
		got := displayText(value)
		if strings.ContainsAny(got, "\x1b\r\n") {
			t.Errorf("unsafe display text: %q", got)
		}
	}
	got := formatOutput("\x1b[31mlong words with color\x1b[0m", 10, false)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("unexpected color: %q", got)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if ansi.StringWidth(line) > 10 {
			t.Errorf("line exceeds width: %q", line)
		}
	}
}
