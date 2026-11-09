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
//	    [--authors <csv>] [--trusted-authors <csv>] [--include-self]
//	    [--include-outdated] [--since <ref|RFC3339>] [--since-last-commit]
//	    [--since-last-push]
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

const usageText = `Usage: pr-review-parser <pr-number> [flags]

Flags:
  --format json|consensus   Output format (default: json)
  --include-resolved        Include resolved threads (default: exclude)
  --authors <csv>           Only include root comments from these logins (default: all reviewers)
  --trusted-authors <csv>   Trusted author logins (agent prompts are ONLY emitted for these exact logins, matched case-insensitively after trim; there is no suffix-based or hardcoded trust).
                            Pass your review bot's exact login (e.g. miracodeai-bot) to receive its agent prompts — without it, NO author's agent prompts are emitted.
  --include-self            Include comments authored by the authenticated user (default: exclude)
  --include-outdated        Include outdated threads (default: exclude)
  --since <ref|RFC3339>     Only comments created after this git ref's commit time or timestamp
  --since-last-commit       Only comments created after HEAD
  --since-last-push         Only comments created after the upstream tracking branch (@{u})
  --help, -h                Show this help`

func printUsage() {
	fmt.Fprintln(os.Stderr, usageText)
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

	const (
		usageLine = "Usage: pr-review-parser <pr-number> [--format json|consensus] [--include-resolved] [--authors <csv>] [--trusted-authors <csv>] [--include-self] [--include-outdated] [--since <ref|RFC3339>] [--since-last-commit] [--since-last-push]"
	)

	format := "json"
	includeResolved := false
	includeSelf := false
	includeOutdated := false
	var authors []string
	var trustedAuthors []string
	var sinceFlag string
	sinceLastCommit := false
	sinceLastPush := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
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

	ctx := context.Background()
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

	// Self-exclusion is fail-closed: if the login cannot be determined,
	// error out (with a pointer at --include-self) instead of silently
	// passing comments through unfiltered — on Forgejo, replies resurface
	// as roots, so an unfiltered set would include the agent's own
	// comments. Users who intentionally want unfiltered output opt in
	// explicitly with --include-self.
	var selfLogin string
	if !includeSelf {
		selfLogin, err = review.ResolveSelfLogin(ctx, platform)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	var rawComments []review.RawComment
	switch platform {
	case review.PlatformGitHub:
		rawComments, err = review.FetchGitHubComments(ctx, owner, repo, prNumber)
	case review.PlatformForgejo:
		rawComments, err = review.FetchForgejoComments(ctx, owner, repo, prNumber)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching comments: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "- Fetched %d total review comments\n", len(rawComments))

	// The shared relevance pipeline (internal/review) owns the root/author,
	// resolved, outdated, and self stages; no baseline capture here.
	res := review.RelevantComments(rawComments, review.RelevantOptions{
		Authors:         authors,
		TrustedAuthors:  trustedAuthors,
		IncludeResolved: includeResolved,
		IncludeOutdated: includeOutdated,
		SelfLogin:       selfLogin,
	}, nil)
	parsed := res.Comments

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

// parseCSVLogins is a thin wrapper over the shared review.ParseCSVLogins
// contract (one site owns the parsing rules).
func parseCSVLogins(flag, val string) []string {
	return review.ParseCSVLogins(flag, val)
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
