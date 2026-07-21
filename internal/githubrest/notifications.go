package githubrest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const restBaseURL = "https://api.github.com"

// Thread is a GitHub notification thread (subset of the REST response).
type Thread struct {
	ID      string `json:"id"`
	Unread  bool   `json:"unread"`
	Subject struct {
		Title string  `json:"title"`
		URL   *string `json:"url"`
		Type  string  `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Client is a minimal REST client for the GitHub Notifications API.
// Notifications are not exposed by the GraphQL API, so listing threads and
// marking them as done must go through REST.
type Client struct {
	http    *http.Client
	BaseURL string
}

func NewClient(httpClient *http.Client) *Client {
	return &Client{http: httpClient, BaseURL: restBaseURL}
}

// ListPullRequestThreads returns notification threads whose subject is a pull
// request. When includeRead is false, only unread threads are returned.
//
// Pagination follows the Link header (rel="next"): the notifications endpoint
// caps per_page at 50, and GitHub may clamp it further, so counting items per
// page is not a reliable termination condition.
func (c *Client) ListPullRequestThreads(ctx context.Context, includeRead bool) ([]Thread, error) {
	var threads []Thread
	next := fmt.Sprintf("%s/notifications?all=%t&per_page=50", c.BaseURL, includeRead)
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		res, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list notifications: %w", err)
		}
		var batch []Thread
		err = json.NewDecoder(res.Body).Decode(&batch)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list notifications: unexpected status %s", res.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("decode notifications: %w", err)
		}
		for _, t := range batch {
			if t.Subject.Type == "PullRequest" {
				threads = append(threads, t)
			}
		}
		next = nextLink(res.Header.Get("Link"))
	}
	return threads, nil
}

// nextLink extracts the rel="next" URL from an RFC 5988 Link header.
func nextLink(header string) string {
	for _, value := range splitLinkValues(header) {
		url, params, found := strings.Cut(value, ">")
		if !found {
			continue
		}
		url = strings.TrimPrefix(strings.TrimSpace(url), "<")
		for _, attr := range strings.Split(params, ";") {
			if strings.TrimSpace(attr) == `rel="next"` {
				return url
			}
		}
	}
	return ""
}

// splitLinkValues splits a Link header into link-values. Commas inside an
// angle-bracketed URL do not act as separators.
func splitLinkValues(header string) []string {
	var values []string
	inURL := false
	start := 0
	for i := range len(header) {
		switch header[i] {
		case '<':
			inURL = true
		case '>':
			inURL = false
		case ',':
			if !inURL {
				values = append(values, header[start:i])
				start = i + 1
			}
		}
	}
	return append(values, header[start:])
}

// MarkThreadDone marks a notification thread as done (removes it from the inbox).
func (c *Client) MarkThreadDone(ctx context.Context, threadID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/notifications/threads/"+threadID, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mark thread %s done: %w", threadID, err)
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil // already gone from the inbox
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("mark thread %s done: unexpected status %s", threadID, res.Status)
	}
	return nil
}
