package githubrest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListPullRequestThreads_PaginatesBeyondFirstPage verifies that more than
// one page (per_page max is 50 on the notifications endpoint) is fetched by
// following the Link header, and that non-PR threads are filtered out.
func TestListPullRequestThreads_PaginatesBeyondFirstPage(t *testing.T) {
	const page1Count = 50
	const page2Count = 7

	thread := func(i int, subjectType string) Thread {
		var th Thread
		th.ID = fmt.Sprintf("%d", i)
		th.Subject.Type = subjectType
		th.Subject.Title = fmt.Sprintf("thread %d", i)
		url := fmt.Sprintf("https://api.github.com/repos/owner/repo/pulls/%d", i+1)
		th.Subject.URL = &url
		th.Repository.FullName = "owner/repo"
		return th
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("all"); got != "true" {
			t.Errorf("expected all=true, got %q", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "50" {
			t.Errorf("expected per_page=50, got %q", got)
		}
		var batch []Thread
		switch r.URL.Query().Get("page") {
		case "", "1":
			for i := range page1Count {
				batch = append(batch, thread(i, "PullRequest"))
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/notifications?page=2&per_page=50&all=true>; rel="next"`, srv.URL))
		case "2":
			for i := range page2Count {
				batch = append(batch, thread(page1Count+i, "PullRequest"))
			}
			// Non-PR threads must be filtered out.
			batch = append(batch, thread(1000, "Issue"))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
		_ = json.NewEncoder(w).Encode(batch)
	}))
	defer srv.Close()

	c := &Client{http: srv.Client(), BaseURL: srv.URL}
	threads, err := c.ListPullRequestThreads(context.Background(), true)
	if err != nil {
		t.Fatalf("ListPullRequestThreads: %v", err)
	}
	if want := page1Count + page2Count; len(threads) != want {
		t.Fatalf("got %d threads, want %d", len(threads), want)
	}
	for i, th := range threads {
		if th.Subject.Type != "PullRequest" {
			t.Errorf("non-PR thread leaked: %+v", th)
		}
		// Values and page order must be preserved end to end.
		if th.ID != fmt.Sprintf("%d", i) {
			t.Errorf("threads[%d].ID = %q", i, th.ID)
		}
		if th.Subject.URL == nil || *th.Subject.URL != fmt.Sprintf("https://api.github.com/repos/owner/repo/pulls/%d", i+1) {
			t.Errorf("threads[%d].Subject.URL = %v", i, th.Subject.URL)
		}
		if th.Repository.FullName != "owner/repo" {
			t.Errorf("threads[%d].Repository.FullName = %q", i, th.Repository.FullName)
		}
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{`<https://api.github.com/notifications?page=2>; rel="next", <https://api.github.com/notifications?page=5>; rel="last"`, "https://api.github.com/notifications?page=2"},
		{`<https://api.github.com/notifications?cursor=a,b>; type="application/json"; rel="next"`, "https://api.github.com/notifications?cursor=a,b"},
		{`<https://api.github.com/notifications?page=1>; rel="prev"`, ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := nextLink(tt.header); got != tt.want {
			t.Errorf("nextLink(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestMarkThreadDone(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"success 205", http.StatusResetContent, false},
		{"not found is idempotent success", http.StatusNotFound, false},
		{"server error fails", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method = %s, want DELETE", r.Method)
				}
				if r.URL.Path != "/notifications/threads/42" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := &Client{http: srv.Client(), BaseURL: srv.URL}
			err := c.MarkThreadDone(context.Background(), "42")
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "42") {
				t.Errorf("error should contain thread id: %v", err)
			}
		})
	}
}

func TestListPullRequestThreads_UnreadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("all"); got != "false" {
			t.Errorf("expected all=false, got %q", got)
		}
		_ = json.NewEncoder(w).Encode([]Thread{})
	}))
	defer srv.Close()

	c := &Client{http: srv.Client(), BaseURL: srv.URL}
	if _, err := c.ListPullRequestThreads(context.Background(), false); err != nil {
		t.Fatalf("ListPullRequestThreads: %v", err)
	}
}

func TestListPullRequestThreads_Errors(t *testing.T) {
	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := &Client{http: srv.Client(), BaseURL: srv.URL}
		if _, err := c.ListPullRequestThreads(context.Background(), true); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		c := &Client{http: srv.Client(), BaseURL: srv.URL}
		if _, err := c.ListPullRequestThreads(context.Background(), true); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("empty page still follows next link", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				var th Thread
				th.ID = "99"
				th.Subject.Type = "PullRequest"
				_ = json.NewEncoder(w).Encode([]Thread{th})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/notifications?page=2>; rel="next"`, srv.URL))
			_ = json.NewEncoder(w).Encode([]Thread{})
		}))
		defer srv.Close()
		c := &Client{http: srv.Client(), BaseURL: srv.URL}
		threads, err := c.ListPullRequestThreads(context.Background(), true)
		if err != nil {
			t.Fatalf("ListPullRequestThreads: %v", err)
		}
		if len(threads) != 1 || threads[0].ID != "99" {
			t.Fatalf("got %+v, want single thread with ID 99", threads)
		}
	})
}
