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
  --trusted-authors <csv>   Additional trusted author logins (agent prompts are only emitted for trusted authors: bot-suffixed logins or these entries).
                            Warning: ANY login ending in 'bot' (case-insensitive) is trusted automatically — a human reviewer with such a login has their agent prompts emitted, and there is no flag to un-trust them.
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
		parsed = append(parsed, review.ParseCommentTrusted(c, trustedAuthors))
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
		var err error
		parsed, err = excludeSelf(parsed, platform)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
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

// excludeSelf drops parsed comments authored by the authenticated user. It
// fails closed: if the login cannot be determined, it returns an error
// instead of silently passing comments through unfiltered — on Forgejo,
// replies resurface as roots, so an unfiltered set would include the
// agent's own comments. Users who intentionally want unfiltered output opt
// in explicitly with --include-self.
func excludeSelf(comments []review.ParsedComment, platform review.Platform) ([]review.ParsedComment, error) {
	selfLogin, err := selfLoginFunc(platform)
	if err != nil {
		return nil, fmt.Errorf("could not determine authenticated user: %w (pass --include-self to skip self-comment exclusion)", err)
	}
	if selfLogin == "" {
		return nil, fmt.Errorf("could not determine authenticated user (empty login) (pass --include-self to skip self-comment exclusion)")
	}
	before := len(comments)
	filtered := filterOutSelf(comments, selfLogin)
	if removed := before - len(filtered); removed > 0 {
		fmt.Fprintf(os.Stderr, "- Excluded %d self-authored comment(s) (author: %s)\n", removed, selfLogin)
	}
	return filtered, nil
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
