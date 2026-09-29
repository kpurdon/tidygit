package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAssociatedPRs(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
		wantErr        bool
	}{
		{"unknown unpushed commit", `{"data":{"repository":{"object":null}}}`, 0, false},
		{"missing repository", `{"data":{"repository":null}}`, 0, true},
		{"graphql error", `{"errors":[{"message":"denied"}],"data":{"repository":null}}`, 0, true},
		{"truncated", `{"data":{"repository":{"object":{"associatedPullRequests":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}}}`, 0, true},
		{"fork identity", `{"data":{"repository":{"object":{"associatedPullRequests":{"nodes":[{"number":7,"state":"MERGED","headRefName":"feature","headRefOid":"abc","headRepository":{"name":"repo","owner":{"login":"fork"}},"mergeCommit":{"oid":"merged"}}],"pageInfo":{"hasNextPage":false}}}}}}`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := associatedPRs(t.Context(), func(_ context.Context, name string, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				if name != "gh" || !strings.Contains(joined, "--hostname github.example") || !strings.Contains(joined, "oid=abc") {
					t.Fatalf("incorrect explicit query: %s %s", name, joined)
				}
				return tc.response, nil
			}, "github.example/owner/repo", "abc", options{PRLimit: 10})
			if (err != nil) != tc.wantErr || len(got) != tc.want {
				t.Fatalf("got %+v, %v", got, err)
			}
			if len(got) > 0 && (got[0].HeadRepo != "github.example/fork/repo" || got[0].MergeCommit == nil || got[0].MergeCommit.OID != "merged" || got[0].Number != 7) {
				t.Fatalf("lost PR identity: %+v", got)
			}
		})
	}
}

func TestConventionalPRCheckoutHint(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
		wantErr        bool
	}{
		{"pr387", `{"data":{"repository":{"pullRequest":{"number":387,"headRefName":"real-branch","headRefOid":"abc","state":"CLOSED","headRepository":{"name":"repo","owner":{"login":"me"}}}}}}`, 387, false},
		{"pr-387", `{"data":{"repository":{"pullRequest":null}}}`, 0, false},
		{"pr1", `{"errors":[{"type":"NOT_FOUND","message":"no PR","path":["repository","pullRequest"]}],"data":{"repository":{"pullRequest":null}}}`, 0, false},
		{"pr2", `{"errors":[{"type":"FORBIDDEN","message":"denied","path":["repository","pullRequest"]}],"data":{"repository":{"pullRequest":null}}}`, 0, true},
		{"pr3", `{"errors":[{"type":"NOT_FOUND","message":"no repository","path":["repository"]}],"data":{"repository":null}}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := conventionalPR(t.Context(), func(_ context.Context, _ string, args ...string) (string, error) {
				if !strings.Contains(strings.Join(args, " "), "--hostname github.example") {
					t.Fatal("missing explicit host")
				}
				if strings.Contains(tc.response, `"errors"`) {
					return tc.response, errors.New("gh graphql error")
				}
				return tc.response, nil
			}, "github.example/owner/repo", tc.name)
			if (err != nil) != tc.wantErr || got.Number != tc.want {
				t.Fatalf("got %+v, %v", got, err)
			}
			if got.Number != 0 && (got.HeadRepo != "github.example/me/repo" || got.HeadOID != "abc" || got.VerifiedAlias) {
				t.Fatalf("hint was not preserved as unverified: %+v", got)
			}
		})
	}
	for _, name := range []string{"pr0", "pr-0", "review-pr-387", "PR387", "pr387-extra", "my/pr387"} {
		_, err := conventionalPR(t.Context(), func(context.Context, string, ...string) (string, error) {
			t.Fatalf("nonconventional branch queried: %s", name)
			return "", nil
		}, "github.example/owner/repo", name)
		if err != nil {
			t.Fatal(err)
		}
	}
}
