package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gqlgo/gqlgenc/clientv2"

	"gh-ignore-bot/internal/githubgraphql"
	"gh-ignore-bot/internal/githubrest"
)

func TestParseConfig(t *testing.T) {
	t.Run("token precedence", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "primary")
		t.Setenv("GH_TOKEN", "fallback")
		cfg, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.token != "primary" {
			t.Errorf("token = %q, want primary", cfg.token)
		}
	})

	t.Run("GH_TOKEN fallback", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "fallback")
		cfg, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.token != "fallback" {
			t.Errorf("token = %q, want fallback", cfg.token)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		if _, err := parseConfig(nil); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("default bots", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "x")
		cfg, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		for _, want := range []string{"renovate", "dependabot"} {
			if !cfg.bots[want] {
				t.Errorf("default bots should contain %q", want)
			}
		}
	})

	t.Run("--bot overrides defaults", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "x")
		cfg, err := parseConfig([]string{"--bot", "my-app[bot]"})
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if !cfg.bots["my-app"] {
			t.Error("bots should contain my-app")
		}
		if cfg.bots["renovate"] {
			t.Error("--bot should replace the defaults")
		}
	})

	t.Run("positional arguments are rejected", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "x")
		// Without rejection, flag parsing stops here and --dry-run would be
		// silently ignored, turning a check into a real run.
		if _, err := parseConfig([]string{"renovate[bot]", "--dry-run"}); err == nil {
			t.Fatal("expected error for positional arguments, got nil")
		}
	})

	t.Run("--help returns ErrHelp", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "x")
		if _, err := parseConfig([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("expected flag.ErrHelp, got %v", err)
		}
	})
}

func TestNormalizeLogin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"renovate[bot]", "renovate"},
		{"renovate", "renovate"},
		{"Renovate[Bot]", "renovate"},
		{"MY-APP[bot]", "my-app"},
		{"takumi3488", "takumi3488"},
	}
	for _, tt := range tests {
		if got := normalizeLogin(tt.in); got != tt.want {
			t.Errorf("normalizeLogin(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseSubjectURL(t *testing.T) {
	url := func(s string) *string { return &s }
	tests := []struct {
		name                string
		url                 *string
		wantOK              bool
		wantOwner, wantName string
		wantNumber          int
	}{
		{"valid", url("https://api.github.com/repos/o/r/pulls/123"), true, "o", "r", 123},
		{"nil", nil, false, "", "", 0},
		{"different host", url("https://example.com/repos/o/r/pulls/1"), false, "", "", 0},
		{"issues path", url("https://api.github.com/repos/o/r/issues/1"), false, "", "", 0},
		{"zero number", url("https://api.github.com/repos/o/r/pulls/0"), false, "", "", 0},
		{"exceeds graphql int32", url("https://api.github.com/repos/o/r/pulls/2147483648"), false, "", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, name, number, ok := parseSubjectURL(tt.url)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (owner != tt.wantOwner || name != tt.wantName || number != tt.wantNumber) {
				t.Errorf("got %s/%s#%d, want %s/%s#%d", owner, name, number, tt.wantOwner, tt.wantName, tt.wantNumber)
			}
		})
	}
}

// fakeGQL implements githubgraphql.GraphQLClient for tests.
type fakeGQL struct {
	calls int
	login string
	title string
	pr    *githubgraphql.GetPullRequestAuthor
	err   error
}

func (f *fakeGQL) GetPullRequestAuthor(_ context.Context, _, _ string, _ int, _ ...clientv2.RequestInterceptor) (*githubgraphql.GetPullRequestAuthor, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.pr != nil {
		return f.pr, nil
	}
	return &githubgraphql.GetPullRequestAuthor{
		Repository: &githubgraphql.GetPullRequestAuthor_Repository{
			PullRequest: &githubgraphql.GetPullRequestAuthor_Repository_PullRequest{
				Title:  f.title,
				Author: &githubgraphql.GetPullRequestAuthor_Repository_PullRequest_Author{Login: f.login},
			},
		},
	}, nil
}

func prThread(id, subjectURL string) githubrest.Thread {
	var t githubrest.Thread
	t.ID = id
	t.Subject.Type = "PullRequest"
	t.Subject.URL = &subjectURL
	t.Repository.FullName = "owner/repo"
	return t
}

func TestResolveAuthor(t *testing.T) {
	cfg := config{bots: map[string]bool{"renovate": true}}
	const validURL = "https://api.github.com/repos/owner/repo/pulls/1"

	t.Run("bot author matches", func(t *testing.T) {
		g := &fakeGQL{login: "renovate[bot]", title: "Update deps"}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", validURL))
		if o.status != statusMatch || o.login != "renovate[bot]" || o.title != "Update deps" {
			t.Errorf("got %+v, want match", o)
		}
	})

	t.Run("case-insensitive bot match", func(t *testing.T) {
		g := &fakeGQL{login: "Renovate[bot]"}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", validURL))
		if o.status != statusMatch {
			t.Errorf("got %+v, want match", o)
		}
	})

	t.Run("human author is skipped", func(t *testing.T) {
		g := &fakeGQL{login: "takumi3488"}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", validURL))
		if o.status != statusSkip {
			t.Errorf("got %+v, want skip", o)
		}
	})

	t.Run("unparseable URL skips without GraphQL call", func(t *testing.T) {
		g := &fakeGQL{}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", "https://api.github.com/repos/owner/repo/issues/1"))
		if o.status != statusSkip {
			t.Errorf("got %+v, want skip", o)
		}
		if g.calls != 0 {
			t.Errorf("GraphQL called %d times for unparseable URL", g.calls)
		}
	})

	t.Run("graphql error becomes statusError", func(t *testing.T) {
		g := &fakeGQL{err: errors.New("rate limited")}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", validURL))
		if o.status != statusError {
			t.Errorf("got %+v, want error", o)
		}
	})

	t.Run("missing pull request is skipped", func(t *testing.T) {
		g := &fakeGQL{pr: &githubgraphql.GetPullRequestAuthor{Repository: &githubgraphql.GetPullRequestAuthor_Repository{}}}
		o := resolveAuthor(context.Background(), g, cfg, prThread("1", validURL))
		if o.status != statusSkip {
			t.Errorf("got %+v, want skip", o)
		}
	})
}

// TestRunAggregatesFailures verifies that per-thread API failures make run
// return a non-nil error while still processing the remaining threads.
func TestRunAggregatesFailures(t *testing.T) {
	const validURL = "https://api.github.com/repos/owner/repo/pulls/1"
	threads := []githubrest.Thread{prThread("1", validURL), prThread("2", validURL)}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(threads)
	}))
	defer srv.Close()

	rest := githubrest.NewClient(srv.Client())
	rest.BaseURL = srv.URL

	t.Run("author fetch failures", func(t *testing.T) {
		g := &fakeGQL{err: errors.New("boom")}
		err := run(context.Background(), config{bots: map[string]bool{"renovate": true}}, rest, g)
		if err == nil {
			t.Fatal("expected aggregated error, got nil")
		}
		if g.calls != len(threads) {
			t.Errorf("GraphQL calls = %d, want %d (all threads still processed)", g.calls, len(threads))
		}
	})

	t.Run("all successes return nil", func(t *testing.T) {
		g := &fakeGQL{login: "someone-else"} // skipped, not a failure
		if err := run(context.Background(), config{bots: map[string]bool{"renovate": true}}, rest, g); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}
