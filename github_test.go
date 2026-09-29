package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubRepo(t *testing.T) {
	for _, raw := range []string{"git@GitHub.com:Owner/Repo.git", "github.com:Owner/Repo.git", "ssh://git@GITHUB.com/Owner/Repo.git", "https://github.com/Owner/Repo.git"} {
		got, err := githubRepo(raw)
		if err != nil || got != "github.com/owner/repo" {
			t.Fatalf("githubRepo(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"../owner/repo", "/tmp/owner/repo", "file:///tmp/owner/repo", "owner/repo"} {
		if got, err := githubRepo(raw); err == nil {
			t.Errorf("accepted local remote %q as %q", raw, got)
		}
	}
}

func TestFetchPRIdentityAndActiveHistory(t *testing.T) {
	branch := Branch{Name: "local", SourceRepo: "github.com/me/repo", SourceBranch: "feature", OID: "abc"}
	merged := `{"number":1,"headRefName":"feature","headRefOid":"abc","state":"MERGED","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"me"}}`
	open := `{"number":2,"headRefName":"feature","headRefOid":"def","state":"OPEN","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"me"}}`
	wrongFork := `{"number":3,"headRefName":"feature","headRefOid":"abc","state":"MERGED","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"other"}}`
	for _, tc := range []struct {
		name, batch, targeted string
		want                  int
	}{
		{"active branch supersedes merged history", "[" + merged + "]", "[" + merged + "," + open + "]", 2},
		{"another fork cannot authorize deletion", "[" + wrongFork + "]", "[" + wrongFork + "]", 0},
		{"missing source metadata cannot authorize deletion", `[{"number":4,"headRefName":"feature","state":"MERGED"}]`, `[{"number":4,"headRefName":"feature","state":"MERGED"}]`, 0},
		{"targeted lookup recovers filtered PR", "[]", "[" + merged + "]", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targeted := false
			command := func(_ context.Context, name string, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				if name != "gh" || !strings.Contains(joined, "--repo github.com/upstream/repo") {
					t.Fatalf("missing explicit target: %s %s", name, joined)
				}
				if strings.Contains(joined, "--head feature") {
					targeted = true
					if strings.Contains(joined, "--author") {
						t.Fatal("targeted query retained author filter")
					}
					return tc.targeted, nil
				}
				return tc.batch, nil
			}
			got, err := fetchPRs(t.Context(), command, "github.com/upstream/repo", []Branch{branch}, options{PRLimit: 10, PRAuthor: "@me"})
			if err != nil {
				t.Fatal(err)
			}
			if !targeted {
				t.Fatal("candidate was not independently verified")
			}
			if got[branch.Name].Number != tc.want {
				t.Fatalf("got %+v; want PR %d", got, tc.want)
			}
		})
	}
}

func TestFetchPRFailures(t *testing.T) {
	branch := Branch{Name: "feature", SourceRepo: "github.com/me/repo", SourceBranch: "feature"}
	failure := errors.New("authentication failed")
	for _, targeted := range []bool{false, true} {
		calls := 0
		_, err := fetchPRs(t.Context(), func(context.Context, string, ...string) (string, error) {
			calls++
			if targeted && calls == 1 {
				return "[]", nil
			}
			return "", failure
		}, "github.com/upstream/repo", []Branch{branch}, options{PRLimit: 10})
		if !errors.Is(err, failure) {
			t.Fatalf("query failure lost: %v", err)
		}
	}
	calls := 0
	_, err := fetchPRs(t.Context(), func(context.Context, string, ...string) (string, error) {
		calls++
		if calls == 1 {
			return "[]", nil
		}
		return `[{"number":1,"headRefName":"feature","state":"MERGED","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"me"}}]`, nil
	}, "github.com/upstream/repo", []Branch{branch}, options{PRLimit: 1})
	if err == nil || !strings.Contains(err.Error(), "reached limit") {
		t.Fatalf("truncated history accepted: %v", err)
	}
}

func TestFetchPREmptyBranches(t *testing.T) {
	got, err := fetchPRs(t.Context(), func(context.Context, string, ...string) (string, error) {
		t.Fatal("queried empty branch set")
		return "", nil
	}, "github.com/me/repo", nil, options{})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFetchPRBoundedConcurrentLookups(t *testing.T) {
	branches := []Branch{
		{Name: "a", SourceRepo: "github.com/me/repo", SourceBranch: "a"},
		{Name: "a-copy", SourceRepo: "github.com/me/repo", SourceBranch: "a"},
		{Name: "b", SourceRepo: "github.com/me/repo", SourceBranch: "b"},
		{Name: "c", SourceRepo: "github.com/me/repo", SourceBranch: "c"},
	}
	var mu sync.Mutex
	calls := make(map[string]int)
	active, peak := 0, 0
	entered := make(chan struct{}, len(branches))
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := fetchPRs(t.Context(), func(ctx context.Context, _ string, args ...string) (string, error) {
			head := ""
			for i, arg := range args {
				if arg == "--head" {
					head = args[i+1]
				}
			}
			if head == "" {
				return "[]", nil
			}
			mu.Lock()
			calls[head]++
			active++
			peak = max(peak, active)
			mu.Unlock()
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			mu.Lock()
			active--
			mu.Unlock()
			return "[]", nil
		}, "github.com/upstream/repo", branches, options{PRLimit: 10, Jobs: 2})
		done <- err
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("targeted queries did not run concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
	if len(calls) != 3 || calls["a"] != 1 {
		t.Fatalf("duplicate source lookup: %v", calls)
	}
}

func TestFetchPRUnknownSources(t *testing.T) {
	branches := []Branch{{Name: "local"}, {Name: "missing-head", SourceRepo: "github.com/me/repo"}}
	got, err := ghFetchPRs(t.Context(), repository{Dir: t.TempDir()}, branches, options{})
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown branch identity triggered external lookup: %v, %v", got, err)
	}
}
