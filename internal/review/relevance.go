package review

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// RelevantOptions selects which filter stages RelevantComments applies.
// pr-review-parser maps its flags onto the Include* stages; pr-review-watch
// always excludes resolved and outdated threads and passes the resolved
// self-login directly (its --include-self flag is resolved to a login
// before the call).
type RelevantOptions struct {
	// Authors restricts the set to root comments from these logins
	// (case-insensitive exact match). Empty means all authors.
	Authors []string
	// TrustedAuthors is forwarded to ParseCommentTrusted and gates agent
	// prompts.
	TrustedAuthors []string
	// IncludeResolved keeps resolved threads (default: exclude).
	IncludeResolved bool
	// IncludeOutdated keeps outdated threads (default: exclude).
	IncludeOutdated bool
	// SelfLogin, when non-empty, drops comments authored by that login
	// (case-insensitive).
	SelfLogin string
}

// RelevantResult carries the pipeline output. The baseline identities of the
// thread roots (captured BEFORE the mutable resolved/outdated/self stages)
// are written to the caller's baselineOut slice, not returned here;
// pr-review-watch uses that slice to make pre-existing threads — including
// ones that are resolved, outdated, or self-authored at start — never
// trigger success, so a thread resolved at start and later reopened does not
// read as a new comment.
type RelevantResult struct {
	Comments []ParsedComment
}

// RelevantComments is the single relevance pipeline shared by
// pr-review-parser and pr-review-watch:
//
//  1. root-only + author filter
//  2. parse (trusted authors)
//  3. resolved filter (skipped when IncludeResolved)
//  4. outdated filter (skipped when IncludeOutdated)
//  5. self filter (skipped when SelfLogin == "")
//
// Stage messages mirror pr-review-parser's historical stderr output for the
// one-shot parser path; the watch path reuses the same stages per poll. When
// baselineOut is non-nil, the raw thread-root IDs from stage 1 are written
// there before stages 3-5 run.
func RelevantComments(raw []RawComment, opts RelevantOptions, baselineOut *[]string) RelevantResult {
	roots := FilterRootComments(raw, opts.Authors...)
	if baselineOut != nil {
		ids := make([]string, len(roots))
		for i, c := range roots {
			ids[i] = c.ID
		}
		*baselineOut = ids
	}

	parsed := make([]ParsedComment, 0, len(roots))
	for _, c := range roots {
		parsed = append(parsed, ParseCommentTrusted(c, opts.TrustedAuthors))
	}

	if !opts.IncludeResolved {
		open := make([]ParsedComment, 0, len(parsed))
		for _, c := range parsed {
			if !c.IsResolved {
				open = append(open, c)
			}
		}
		parsed = open
		fmt.Fprintf(os.Stderr, "- After filtering resolved: %d open comments\n", len(parsed))
	}

	if !opts.IncludeOutdated {
		current := make([]ParsedComment, 0, len(parsed))
		for _, c := range parsed {
			if !c.IsOutdated {
				current = append(current, c)
			}
		}
		parsed = current
		fmt.Fprintf(os.Stderr, "- After filtering outdated: %d current comments\n", len(parsed))
	}

	if opts.SelfLogin != "" {
		parsed = filterSelf(parsed, opts.SelfLogin)
	}

	return RelevantResult{Comments: parsed}
}

// SelfLoginFunc resolves the authenticated login; an exported seam so
// binaries (and their tests) can stub SelfLogin without hitting gh or the
// Forgejo API.
var SelfLoginFunc = SelfLogin

// ResolveSelfLogin resolves the authenticated login and fails closed: a
// failure or empty login is an error (with a pointer at --include-self)
// instead of silently passing comments through unfiltered — on Forgejo,
// replies resurface as roots, so an unfiltered set would include the
// agent's own comments.
func ResolveSelfLogin(ctx context.Context, platform Platform) (string, error) {
	login, err := SelfLoginFunc(ctx, platform)
	if err != nil {
		return "", fmt.Errorf("could not determine authenticated user: %w (pass --include-self to skip self-comment exclusion)", err)
	}
	if login == "" {
		return "", fmt.Errorf("could not determine authenticated user (empty login) (pass --include-self to skip self-comment exclusion)")
	}
	return login, nil
}

// filterSelf drops parsed comments authored by selfLogin (case-insensitive).
func filterSelf(comments []ParsedComment, selfLogin string) []ParsedComment {
	out := make([]ParsedComment, 0, len(comments))
	for _, c := range comments {
		if strings.EqualFold(c.Author, selfLogin) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ParseCSVLogins splits a comma-separated author login list into trimmed
// non-empty logins. GitHub/Forgejo logins cannot start with "-", so reject
// "--"-prefixed tokens to catch misplaced flags.
func ParseCSVLogins(flag, val string) []string {
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
