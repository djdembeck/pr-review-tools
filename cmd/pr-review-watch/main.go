// Command pr-review-watch persistently polls a PR for new review comments
// and exits as soon as a new relevant root comment appears. It captures a
// baseline of the currently relevant comments at start; only comments that
// appear after the watch begins count as "a reviewer posted". A new reply
// inside an existing thread never triggers success.
//
// It is reviewer-agnostic: by default new root comments from ANY reviewer
// (bots or humans) count.
//
// Exit codes: 0 = new review comment found; 1 = usage/setup/fetch error;
// 2 = timed out.
//
// Usage:
//
//	pr-review-watch <pr-number> [--timeout 15m] [--poll 30s] [--authors <csv>]
//	    [--trusted-authors <csv>] [--include-self] [--format json|consensus]
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

const (
	defaultTimeout = 15 * time.Minute
	defaultPoll    = 30 * time.Second
	exitTimeout    = 2
)

const usageText = `Usage: pr-review-watch <pr-number> [flags]

Flags:
  --timeout <duration>    Stop waiting after this duration (default: 15m; e.g. 30s, 15m, 1h)
  --poll <duration>       Poll interval (default: 30s)
  --authors <csv>         Only wait for new root comments from these logins (default: all reviewers)
  --trusted-authors <csv> Trusted author logins (agent prompts only emitted for these exact logins)
  --include-self          Include comments authored by the authenticated user (default: exclude)
  --format json|consensus Output format (default: json)
  --help, -h              Show this help

Exit codes: 0 = new review comment found; 1 = usage/setup/fetch error; 2 = timed out.`

func printUsage() {
	fmt.Fprintln(os.Stderr, usageText)
}

// fetchFunc returns the currently relevant parsed comments (raw fetch + all
// filters), or an error. main() builds this closure once.
type fetchFunc func() ([]review.ParsedComment, error)

// watch blocks until fetch yields a comment whose ID is not in baseline,
// the deadline passes, or the process is killed. Returns the new comments
// (fetch order), polls performed, and time waited. The returned error is
// non-nil only on the timeout path.
func watch(baseline []review.ParsedComment, timeout, poll time.Duration, fetch fetchFunc) (newComments []review.ParsedComment, polls int, waited time.Duration, err error) {
	baselineIDs := make(map[string]bool, len(baseline))
	for _, c := range baseline {
		baselineIDs[c.ID] = true
	}

	start := time.Now()
	deadline := start.Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			polled, err := fetch()
			if err != nil {
				fmt.Fprintf(os.Stderr, "- Poll %d failed: %v (retrying)\n", polls+1, err)
				continue
			}
			polls++
			var fresh []review.ParsedComment
			for _, c := range polled {
				if !baselineIDs[c.ID] {
					fresh = append(fresh, c)
				}
			}
			if len(fresh) > 0 {
				fmt.Fprintf(os.Stderr, "- Poll %d: %d new comment(s) found\n", polls, len(fresh))
				return fresh, polls, time.Since(start), nil
			}
			fmt.Fprintf(os.Stderr, "- Poll %d: no new comments (%s elapsed)\n", polls, time.Since(start).Round(time.Second))
		case <-time.After(time.Until(deadline)):
			return nil, polls, time.Since(start), fmt.Errorf("timed out after %s: no new review comments", timeout)
		}
	}
}

// selectRelevant applies the watch's relevant-comment contract to raw
// comments: root-only, author filter, open, not outdated, self-excluded.
func selectRelevant(raw []review.RawComment, authors, trustedAuthors []string, selfLogin string) []review.ParsedComment {
	roots := review.FilterRootComments(raw, authors...)
	parsed := make([]review.ParsedComment, 0, len(roots))
	for _, c := range roots {
		parsed = append(parsed, review.ParseCommentTrusted(c, trustedAuthors))
	}
	out := make([]review.ParsedComment, 0, len(parsed))
	for _, c := range parsed {
		if c.IsResolved || c.IsOutdated {
			continue
		}
		out = append(out, c)
	}
	if selfLogin != "" {
		out = filterOutSelf(out, selfLogin)
	}
	return out
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}
	prNumber, showHelp, headErr := parseHead(args)
	if showHelp {
		printUsage()
		os.Exit(0)
	}
	if headErr != "" {
		fmt.Fprintln(os.Stderr, headErr)
		os.Exit(1)
	}

	const usageLine = "Usage: pr-review-watch <pr-number> [--timeout <duration>] [--poll <duration>] [--authors <csv>] [--trusted-authors <csv>] [--include-self] [--format json|consensus]"

	timeout := defaultTimeout
	poll := defaultPoll
	format := "json"
	includeSelf := false
	var authors []string
	var trustedAuthors []string

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil || d <= 0 {
					fmt.Fprintf(os.Stderr, "Error: --timeout must be a positive duration (e.g. 15m), got '%s'\n", args[i+1])
					os.Exit(1)
				}
				timeout = d
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --timeout requires a value")
				os.Exit(1)
			}
		case "--poll":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil || d <= 0 {
					fmt.Fprintf(os.Stderr, "Error: --poll must be a positive duration (e.g. 30s), got '%s'\n", args[i+1])
					os.Exit(1)
				}
				poll = d
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --poll requires a value")
				os.Exit(1)
			}
		case "--authors":
			if i+1 < len(args) {
				authors = append(authors, parseCSVLogins("authors", args[i+1])...)
				if len(authors) == 0 {
					fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
					os.Exit(1)
				}
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
				os.Exit(1)
			}
		case "--trusted-authors":
			if i+1 < len(args) {
				trustedAuthors = append(trustedAuthors, parseCSVLogins("trusted-authors", args[i+1])...)
				if len(trustedAuthors) == 0 {
					fmt.Fprintln(os.Stderr, "Error: --trusted-authors requires a non-empty value")
					os.Exit(1)
				}
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --trusted-authors requires a non-empty value")
				os.Exit(1)
			}
		case "--include-self":
			includeSelf = true
		case "--format":
			if i+1 < len(args) {
				f := args[i+1]
				if f == "json" || f == "consensus" {
					format = f
					i++
				} else {
					fmt.Fprintf(os.Stderr, "Error: --format must be 'json' or 'consensus', got '%s'\n", f)
					os.Exit(1)
				}
			} else {
				fmt.Fprintln(os.Stderr, "Error: --format requires a value")
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag '%s'\n", args[i])
			fmt.Fprintln(os.Stderr, usageLine)
			os.Exit(1)
		}
	}

	remote, err := review.GetGitRemote()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	platform := review.DetectPlatform(remote)
	owner, repo, err := review.ParseRemoteRepo(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "- Detecting %s repo: %s/%s\n", platform, owner, repo)

	selfLogin := ""
	if !includeSelf {
		selfLogin, err = selfLoginFunc(platform)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not determine authenticated user: %v (pass --include-self to skip self-comment exclusion)\n", err)
			os.Exit(1)
		}
		if selfLogin == "" {
			fmt.Fprintln(os.Stderr, "Error: could not determine authenticated user (empty login) (pass --include-self to skip self-comment exclusion)")
			os.Exit(1)
		}
	}

	fetch := func() ([]review.ParsedComment, error) {
		var raw []review.RawComment
		var err error
		switch platform {
		case review.PlatformGitHub:
			raw, err = review.FetchGitHubComments(owner, repo, prNumber)
		case review.PlatformForgejo:
			raw, err = review.FetchForgejoComments(owner, repo, prNumber)
		}
		if err != nil {
			return nil, err
		}
		return selectRelevant(raw, authors, trustedAuthors, selfLogin), nil
	}

	base, err := fetch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching comments: %v\n", err)
		os.Exit(1)
	}
	label := "any reviewer"
	if len(authors) > 0 {
		label = "authors: " + strings.Join(authors, ", ")
	}
	fmt.Fprintf(os.Stderr, "- Baseline: %d comment(s) already present; waiting for new review comment(s) from %s\n", len(base), label)

	newComments, _, waited, err := watch(base, timeout, poll, fetch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "- Timed out after %s: no new review comments\n", timeout)
		os.Exit(exitTimeout)
	}

	fmt.Fprintf(os.Stderr, "- Found %d new comment(s) after %s\n", len(newComments), waited.Round(time.Second))

	var output string
	if format == "consensus" {
		output = review.FormatConsensus(newComments)
	} else {
		output, err = review.FormatJSON(newComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println(output)
}

// parseHead validates the positional PR number and reports a help request
// without touching os.Exit so it can be unit-tested. It returns the PR number,
// whether the user requested help, and an error message ("" on success).
// --help / -h wins regardless of position or PR-number validity.
func parseHead(args []string) (int, bool, string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return 0, true, ""
		}
	}
	prNumber, err := strconv.Atoi(args[0])
	if err != nil || prNumber <= 0 {
		return 0, false, "Invalid PR number: " + args[0]
	}
	return prNumber, false, ""
}

// parseCSVLogins splits a comma-separated author login list into trimmed
// non-empty logins. GitHub/Forgejo logins cannot start with "-", so reject
// "--"-prefixed tokens to catch misplaced flags.
func parseCSVLogins(flag, val string) []string {
	if strings.HasPrefix(val, "--") {
		fmt.Fprintf(os.Stderr, "Error: %s got flag-shaped value '%s', expected CSV of logins\n", flag, val)
		os.Exit(1)
	}
	var out []string
	for _, a := range strings.Split(val, ",") {
		if a = strings.TrimSpace(a); a != "" {
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(os.Stderr, "Error: %s got flag-shaped login '%s', expected login\n", flag, a)
				os.Exit(1)
			}
			out = append(out, a)
		}
	}
	return out
}

// selfLoginFunc resolves the authenticated login; a seam so tests can stub
// review.SelfLogin without hitting gh or the Forgejo API.
var selfLoginFunc = review.SelfLogin

// filterOutSelf drops parsed comments authored by selfLogin (case-insensitive).
func filterOutSelf(comments []review.ParsedComment, selfLogin string) []review.ParsedComment {
	out := make([]review.ParsedComment, 0, len(comments))
	for _, c := range comments {
		if strings.EqualFold(c.Author, selfLogin) {
			continue
		}
		out = append(out, c)
	}
	return out
}
