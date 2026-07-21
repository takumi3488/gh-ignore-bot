//go:generate go tool gqlgenc

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gqlgo/gqlgenc/clientv2"

	"gh-ignore-bot/internal/githubgraphql"
	"gh-ignore-bot/internal/githubrest"

	"golang.org/x/sync/errgroup"
)

const graphQLEndpoint = "https://api.github.com/graphql"

var defaultBots = []string{"renovate[bot]", "dependabot[bot]"}

// botList is a repeatable --bot flag that overrides the default bot list.
type botList []string

func (b *botList) String() string { return strings.Join(*b, ",") }
func (b *botList) Set(v string) error {
	*b = append(*b, v)
	return nil
}

type config struct {
	token      string
	bots       map[string]bool
	unreadOnly bool
	dryRun     bool
}

func parseConfig(args []string) (config, error) {
	var bots botList
	var unreadOnly, dryRun bool
	fs := flag.NewFlagSet("gh-ignore-bot", flag.ContinueOnError)
	fs.Var(&bots, "bot", "bot login whose PR notifications are marked done (repeatable, default: renovate[bot], dependabot[bot])")
	fs.BoolVar(&unreadOnly, "unread", false, "only process unread notifications (default: all notifications in the inbox)")
	fs.BoolVar(&dryRun, "dry-run", false, "list matching notifications without marking them done")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	if token == "" {
		return config{}, errors.New("GITHUB_TOKEN (or GH_TOKEN) environment variable is required")
	}

	if len(bots) == 0 {
		bots = defaultBots
	}
	set := make(map[string]bool, len(bots))
	for _, b := range bots {
		set[normalizeLogin(b)] = true
	}
	return config{token: token, bots: set, unreadOnly: unreadOnly, dryRun: dryRun}, nil
}

// normalizeLogin strips the "[bot]" suffix so that both "renovate" and
// "renovate[bot]" match: the login returned by the GraphQL API varies
// depending on how the app is installed.
func normalizeLogin(login string) string {
	login = strings.ToLower(login)
	return strings.TrimSuffix(login, "[bot]")
}

// subjectURLPattern matches pull request URLs from notification subjects, e.g.
// https://api.github.com/repos/owner/repo/pulls/123
var subjectURLPattern = regexp.MustCompile(`^https://api\.github\.com/repos/([^/]+)/([^/]+)/pulls/(\d+)$`)

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &authTransport{token: cfg.token, base: http.DefaultTransport},
	}
	rest := githubrest.NewClient(httpClient)
	gql := githubgraphql.NewClient(httpClient, graphQLEndpoint, &clientv2.Options{})
	if err := run(context.Background(), cfg, rest, gql); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, rest *githubrest.Client, gql githubgraphql.GraphQLClient) error {
	threads, err := rest.ListPullRequestThreads(ctx, !cfg.unreadOnly)
	if err != nil {
		return err
	}
	fmt.Printf("found %d pull request notification(s)\n", len(threads))

	// Resolve PR authors concurrently: one GraphQL call per thread is too slow
	// when run sequentially for hundreds of notifications.
	outcomes := make([]outcome, len(threads))
	var g errgroup.Group
	g.SetLimit(8)
	for i, t := range threads {
		g.Go(func() error {
			outcomes[i] = resolveAuthor(ctx, gql, cfg, t)
			return nil
		})
	}
	g.Wait()

	marked, failures := 0, 0
	for i, t := range threads {
		o := outcomes[i]
		ref := t.Repository.FullName
		if _, _, number, ok := parseSubjectURL(t.Subject.URL); ok {
			ref = fmt.Sprintf("%s#%d", ref, number)
		}
		switch o.status {
		case statusSkip:
			fmt.Printf("skip  %s: %s\n", ref, o.message)
		case statusError:
			fmt.Printf("error %s: %s\n", ref, o.message)
			failures++
		case statusMatch:
			if cfg.dryRun {
				fmt.Printf("match %s: %s (author: %s)\n", ref, o.title, o.login)
				marked++
				continue
			}
			if err := rest.MarkThreadDone(ctx, t.ID); err != nil {
				fmt.Printf("error %s: %v\n", ref, err)
				failures++
				continue
			}
			fmt.Printf("done  %s: %s (author: %s)\n", ref, o.title, o.login)
			marked++
		}
	}

	verb := "marked done"
	if cfg.dryRun {
		verb = "would mark done"
	}
	fmt.Printf("%s %d notification(s)", verb, marked)
	if failures > 0 {
		fmt.Printf(", %d failed\n", failures)
		return fmt.Errorf("%d notification(s) failed to process", failures)
	}
	fmt.Println()
	return nil
}

const (
	statusSkip  = "skip"
	statusMatch = "match"
	statusError = "error"
)

type outcome struct {
	status  string
	login   string
	title   string
	message string
}

func resolveAuthor(ctx context.Context, gql githubgraphql.GraphQLClient, cfg config, t githubrest.Thread) outcome {
	owner, name, number, ok := parseSubjectURL(t.Subject.URL)
	if !ok {
		return outcome{status: statusSkip, message: "cannot parse subject URL"}
	}
	pr, err := gql.GetPullRequestAuthor(ctx, owner, name, number)
	if err != nil {
		return outcome{status: statusError, message: "fetch author: " + err.Error()}
	}
	pull := pr.GetRepository().GetPullRequest()
	if pull == nil || pull.GetAuthor() == nil {
		return outcome{status: statusSkip, message: "pull request or author not found"}
	}
	login := pull.GetAuthor().GetLogin()
	if !cfg.bots[normalizeLogin(login)] {
		return outcome{status: statusSkip, message: fmt.Sprintf("author %q is not a target bot", login)}
	}
	return outcome{status: statusMatch, login: login, title: pull.GetTitle()}
}

func parseSubjectURL(url *string) (owner, name string, number int, ok bool) {
	if url == nil {
		return "", "", 0, false
	}
	m := subjectURLPattern.FindStringSubmatch(*url)
	if m == nil {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return "", "", 0, false
	}
	return m[1], m[2], n, true
}

// authTransport injects the GitHub API headers into every request.
type authTransport struct {
	token string
	base  http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-ignore-bot")
	return t.base.RoundTrip(req)
}
