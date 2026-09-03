// Command pr-review-parser fetches PR review comments from GitHub or Forgejo,
// filters them down to the relevant set for an agent review loop (open, not
// outdated, optionally not self-authored, optionally created after a given
// git ref or timestamp), and parses them into structured data (JSON or
// consensus markdown).
//
// It is reviewer-agnostic: by default root comments from ANY reviewer (bots
// or humans) are included.
//
// Usage:
//
//	pr-review-parser <pr-number> [--format json|consensus] [--include-resolved]
//	    [--authors <csv>] [--include-self] [--include-outdated]
//	    [--since <ref|RFC3339>] [--since-last-commit] [--since-last-push]
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `Usage: pr-review-parser <pr-number> [flags]

Flags:
  --format json|consensus   Output format (default: json)
  --include-resolved        Include resolved threads (default: exclude)
  --authors <csv>           Only include root comments from these logins (default: all reviewers)
  --include-self            Include comments authored by the authenticated user (default: exclude)
  --include-outdated        Include outdated threads (default: exclude)
  --since <ref|RFC3339>     Only comments created after this git ref's commit time or timestamp
  --since-last-commit       Only comments created after HEAD
  --since-last-push         Only comments created after the upstream tracking branch (@{u})
  --help, -h                Show this help`)
		os.Exit(1)
	}

	prNumber, err := strconv.Atoi(args[0])
	if err != nil || prNumber <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid PR number: %s\n", args[0])
		os.Exit(1)
	}

	const (
		usageLine = "Usage: pr-review-parser <pr-number> [--format json|consensus] [--include-resolved] [--authors <csv>] [--include-self] [--include-outdated] [--since <ref|RFC3339>] [--since-last-commit] [--since-last-push]"
	)

	format := "json"
	includeResolved := false
	includeSelf := false
	includeOutdated := false
	var authors []string
	var sinceFlag string
	sinceLastCommit := false
	sinceLastPush := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--authors":
			if i+1 < len(args) {
				val := args[i+1]
				// GitHub/Forgejo logins cannot start with "-", so reject
				// "--"-prefixed tokens to catch misplaced flags.
				if strings.HasPrefix(val, "--") {
					fmt.Fprintf(os.Stderr, "Error: --authors got flag-shaped value '%s', expected CSV of author logins\n", val)
					os.Exit(1)
				}
				prev := len(authors)
				for _, a := range strings.Split(val, ",") {
					if a = strings.TrimSpace(a); a != "" {
						if strings.HasPrefix(a, "--") {
							fmt.Fprintf(os.Stderr, "Error: --authors got flag-shaped author '%s', expected login\n", a)
							os.Exit(1)
						}
						authors = append(authors, a)
					}
				}
				if len(authors) == prev {
					fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
					os.Exit(1)
				}
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
				os.Exit(1)
			}
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
		case "--include-resolved":
			includeResolved = true
		case "--include-self":
			includeSelf = true
		case "--include-outdated":
			includeOutdated = true
		case "--since":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				sinceFlag = args[i+1]
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --since requires a git ref or RFC3339 timestamp value")
				os.Exit(1)
			}
		case "--since-last-commit":
			sinceLastCommit = true
		case "--since-last-push":
			sinceLastPush = true
		case "--help", "-h":
			fmt.Fprintln(os.Stderr, "Usage: pr-review-parser <pr-number> [flags]; run with no arguments for full flag help")
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag '%s'\n", args[i])
			fmt.Fprintln(os.Stderr, usageLine)
			os.Exit(1)
		}
	}

	// At most one since source.
	sinceCount := 0
	if sinceFlag != "" {
		sinceCount++
	}
	if sinceLastCommit {
		sinceCount++
	}
	if sinceLastPush {
		sinceCount++
	}
	if sinceCount > 1 {
		fmt.Fprintf(os.Stderr, "Error: only one of --since, --since-last-commit, --since-last-push may be given\n")
		fmt.Fprintln(os.Stderr, usageLine)
		os.Exit(1)
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

	var rawComments []review.RawComment
	switch platform {
	case review.PlatformGitHub:
		rawComments, err = review.FetchGitHubComments(owner, repo, prNumber)
	case review.PlatformForgejo:
		rawComments, err = review.FetchForgejoComments(owner, repo, prNumber)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching comments: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "- Fetched %d total review comments\n", len(rawComments))

	roots := review.FilterRootComments(rawComments, authors...)
	fmt.Fprintf(os.Stderr, "- Found %d review root comments\n", len(roots))

	parsed := make([]review.ParsedComment, 0, len(roots))
	for _, c := range roots {
		parsed = append(parsed, review.ParseComment(c))
	}

	if !includeResolved {
		open := make([]review.ParsedComment, 0, len(parsed))
		for _, c := range parsed {
			if !c.IsResolved {
				open = append(open, c)
			}
		}
		parsed = open
		fmt.Fprintf(os.Stderr, "- After filtering resolved: %d open comments\n", len(parsed))
	}

	if !includeOutdated {
		current := make([]review.ParsedComment, 0, len(parsed))
		for _, c := range parsed {
			if !c.IsOutdated {
				current = append(current, c)
			}
		}
		parsed = current
		fmt.Fprintf(os.Stderr, "- After filtering outdated: %d current comments\n", len(parsed))
	}

	if !includeSelf {
		selfLogin, err := review.SelfLogin(platform)
		if err != nil || selfLogin == "" {
			if err != nil {
				fmt.Fprintf(os.Stderr, "- Warning: could not determine authenticated user (%v); skipping self-comment exclusion\n", err)
			}
		} else {
			before := len(parsed)
			parsed = filterOutSelf(parsed, selfLogin)
			if removed := before - len(parsed); removed > 0 {
				fmt.Fprintf(os.Stderr, "- Excluded %d self-authored comment(s) (author: %s)\n", removed, selfLogin)
			}
		}
	}

	if sinceCount == 1 {
		var since time.Time
		var sinceLabel string
		switch {
		case sinceFlag != "":
			sinceLabel = "--since " + sinceFlag
			since, err = review.ParseSinceArg(sinceFlag)
		case sinceLastCommit:
			sinceLabel = "--since-last-commit"
			since, err = review.GitCommitTime("HEAD")
		case sinceLastPush:
			sinceLabel = "--since-last-push"
			since, err = review.GitCommitTime("@{u}")
			if err != nil {
				err = fmt.Errorf("%w: set an upstream (git push -u) or pass --since <ref|RFC3339>", err)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "- Filtering comments created after %s (%s)\n", since.Format(time.RFC3339), sinceLabel)
		parsed = filterSinceParsed(parsed, since)
		fmt.Fprintf(os.Stderr, "- After since-filter: %d comments\n", len(parsed))
	}

	var output string
	if format == "consensus" {
		output = review.FormatConsensus(parsed)
	} else {
		output, err = review.FormatJSON(parsed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println(output)
}

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

// filterSinceParsed keeps parsed comments created strictly after since.
// Missing or unparseable timestamps fail open (kept) via review.FilterSince.
func filterSinceParsed(comments []review.ParsedComment, since time.Time) []review.ParsedComment {
	raws := make([]review.RawComment, 0, len(comments))
	for _, c := range comments {
		raws = append(raws, review.RawComment{
			ID:         c.ID,
			Author:     c.Author,
			CreatedAt:  c.CreatedAt,
			IsResolved: c.IsResolved,
			IsOutdated: c.IsOutdated,
		})
	}
	keep := review.FilterSince(raws, since)
	keepIDs := make(map[string]bool, len(keep))
	for _, c := range keep {
		keepIDs[c.ID] = true
	}
	out := make([]review.ParsedComment, 0, len(keep))
	for _, c := range comments {
		if keepIDs[c.ID] {
			out = append(out, c)
		}
	}
	return out
}
