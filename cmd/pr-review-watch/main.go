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
//
// With --events, wait durably for all selected reviewers at a pinned PR head.
// Versioned JSON distinguishes readiness, timeout, head drift, closure and errors.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"encoding/json"
	"github.com/djdembeck/pr-review-tools/internal/review"
)

const (
	defaultTimeout = 15 * time.Minute
	defaultPoll    = 30 * time.Second
	exitTimeout    = 2
)

const usageText = `Usage: pr-review-watch <pr-number> [flags]

Waits for reviewers on a pull request. Two modes:

  default      poll for a new relevant root review comment, then exit 0
  --events     durable, head-bound lifecycle wait with a versioned JSON result

Flags:
  --timeout <duration>    Stop waiting after this duration (default: 15m legacy, 30m lifecycle; e.g. 30s, 15m, 1h)
  --poll <duration>       Poll interval (default: 30s)
  --authors <csv>         Only wait for comments/reviews from these logins (default: all reviewers)
  --trusted-authors <csv> Trusted author logins (agent prompts only emitted for these exact logins)
  --include-self          Include comments authored by the authenticated user (default: exclude)
  --format json|consensus Output format, legacy mode only (default: json)
  --events                Enable lifecycle mode (durable state, typed events)
  --state <path>          Lifecycle mode: durable state file (created 0600, atomic writes)
  --head <sha>            Lifecycle mode: PR head SHA that reviews must bind to
  --repo <owner/repo>     Lifecycle mode: receiving repository (host still detected from origin)
  --wait-for all|any      Lifecycle mode: require all selected authors or any one (default: all)
  --help, -h              Show this help

Lifecycle mode prints one JSON status object on stdout and logs progress to
stderr. The marker "review-watch: armed" appears once the baseline is durable,
so CI may start before the reviewers respond.

Exit codes (legacy):    0 = new review comment found; 1 = usage/setup/fetch error; 2 = timed out.
Exit codes (lifecycle): 0 = ready; 1 = setup/fetch/state error; 2 = timed out; 3 = head changed; 4 = PR closed.`

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

func runLegacyMain() {
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

func main() {
	for _, arg := range os.Args[1:] {
		if arg != "--events" {
			continue
		}
		args := os.Args[1:]
		pr, help, headErr := parseHead(args)
		if help {
			printUsage()
			return
		}
		if headErr != "" {
			fmt.Fprintln(os.Stderr, headErr)
			os.Exit(1)
		}
		opts, ok := parseFlags(args, pr)
		if !ok {
			os.Exit(1)
		}
		os.Exit(runLifecycleMode(opts))
	}
	runLegacyMain()
}

const exitHeadChanged = 3

const exitPRClosed = 4

// watchOptions is the parsed shape of every flag, both modes. Lifecycle-only
// fields are rejected unless --events is present, so they can never be
// silently ignored.
type watchOptions struct {
	pr      int
	poll    time.Duration
	timeout time.Duration
	// timeoutSet records an explicit --timeout so the longer lifecycle
	// default never overrides it.
	timeoutSet bool
	format     string
	authors    []string
	trusted    []string
	self       bool

	// Lifecycle mode.
	events  bool
	state   string
	head    string
	repo    string
	waitFor string
}

// parseFlags parses every flag of both modes. It prints the usage error and
// returns false when an invocation must exit 1, so callers stay testable.
func parseFlags(args []string, pr int) (watchOptions, bool) {
	opts := watchOptions{
		pr:      pr,
		timeout: defaultTimeout,
		poll:    defaultPoll,
		format:  "json",
		waitFor: review.WaitForAll,
	}

	// lifecycleFlags and legacyFlags record mode-specific flags so an
	// invocation that mixes the two modes fails closed instead of silently
	// ignoring a flag.
	var lifecycleFlags []string
	var legacyFlags []string
	requiresValue := func(i int, flag string) (string, bool) {
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", flag)
			return "", false
		}
		return args[i+1], true
	}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil || d <= 0 {
					fmt.Fprintf(os.Stderr, "Error: --timeout must be a positive duration (e.g. 15m), got '%s'\n", args[i+1])
					return opts, false
				}
				opts.timeout = d
				opts.timeoutSet = true
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --timeout requires a value")
				return opts, false
			}
		case "--poll":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil || d <= 0 {
					fmt.Fprintf(os.Stderr, "Error: --poll must be a positive duration (e.g. 30s), got '%s'\n", args[i+1])
					return opts, false
				}
				opts.poll = d
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --poll requires a value")
				return opts, false
			}
		case "--authors":
			if i+1 < len(args) {
				opts.authors = append(opts.authors, parseCSVLogins("authors", args[i+1])...)
				if len(opts.authors) == 0 {
					fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
					return opts, false
				}
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --authors requires a non-empty value")
				return opts, false
			}
		case "--trusted-authors":
			if i+1 < len(args) {
				opts.trusted = append(opts.trusted, parseCSVLogins("trusted-authors", args[i+1])...)
				if len(opts.trusted) == 0 {
					fmt.Fprintln(os.Stderr, "Error: --trusted-authors requires a non-empty value")
					return opts, false
				}
				i++
			} else {
				fmt.Fprintln(os.Stderr, "Error: --trusted-authors requires a non-empty value")
				return opts, false
			}
		case "--include-self":
			opts.self = true
			legacyFlags = append(legacyFlags, "--include-self")
		case "--format":
			if i+1 < len(args) {
				f := args[i+1]
				if f == "json" || f == "consensus" {
					opts.format = f
					i++
					legacyFlags = append(legacyFlags, "--format")
				} else {
					fmt.Fprintf(os.Stderr, "Error: --format must be 'json' or 'consensus', got '%s'\n", f)
					return opts, false
				}
			} else {
				fmt.Fprintln(os.Stderr, "Error: --format requires a value")
				return opts, false
			}
		case "--events":
			opts.events = true
		case "--state":
			v, ok := requiresValue(i, "--state")
			if !ok {
				return opts, false
			}
			opts.state = v
			i++
			lifecycleFlags = append(lifecycleFlags, "--state")
		case "--head":
			v, ok := requiresValue(i, "--head")
			if !ok {
				return opts, false
			}
			opts.head = v
			i++
			lifecycleFlags = append(lifecycleFlags, "--head")
		case "--repo":
			v, ok := requiresValue(i, "--repo")
			if !ok {
				return opts, false
			}
			opts.repo = v
			i++
			lifecycleFlags = append(lifecycleFlags, "--repo")
		case "--wait-for":
			v, ok := requiresValue(i, "--wait-for")
			if !ok {
				return opts, false
			}
			if v != review.WaitForAll && v != review.WaitForAny {
				fmt.Fprintf(os.Stderr, "Error: --wait-for must be 'all' or 'any', got '%s'\n", v)
				return opts, false
			}
			opts.waitFor = v
			i++
			lifecycleFlags = append(lifecycleFlags, "--wait-for")
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag '%s'\n", args[i])
			printUsage()
			return opts, false
		}
	}

	if !opts.events {
		// Legacy mode never varies its behavior on a lifecycle flag: an
		// invocation that passes one is a mistake and must fail closed.
		if len(lifecycleFlags) > 0 {
			fmt.Fprintf(os.Stderr, "Error: %s requires --events (lifecycle mode)\n", lifecycleFlags[0])
			return opts, false
		}
		// A poll interval of at least the timeout guarantees the deadline
		// fires before the first tick: zero polls run and the run reads as
		// "no comments in <timeout>". Reject the combination at flag
		// validation (usage error, exit 1). Lifecycle mode needs no such
		// guard: its deadline is enforced inside each poll.
		if opts.poll >= opts.timeout {
			fmt.Fprintf(os.Stderr, "Error: --poll (%s) must be less than --timeout (%s); otherwise the deadline fires before any poll runs\n", opts.poll, opts.timeout)
			return opts, false
		}
	}
	if opts.events && len(legacyFlags) > 0 {
		// Lifecycle mode prints a JSON result and never reprints comments, so
		// a legacy-only flag cannot take effect: refuse it rather than
		// silently ignoring it. (--include-self has no meaning either: the
		// lifecycle selection is the exact --authors set.)
		fmt.Fprintf(os.Stderr, "Error: %s has no effect in lifecycle mode\n", legacyFlags[0])
		return opts, false
	}
	return opts, true
}

// runLifecycleMode executes one durable, head-bound lifecycle round and prints
// its versioned result object.
func runLifecycleMode(opts watchOptions) int {
	remote, err := review.GetGitRemote()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return emitSetupError(newLifecycleEnvelope(opts), err)
	}
	owner, repo, platform, err := resolveTargetFromRemote(remote, opts.repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return emitSetupError(newLifecycleEnvelope(opts), err)
	}

	envelope := newLifecycleEnvelope(opts)
	envelope.Platform = platform
	envelope.Repo = owner + "/" + repo

	backend, err := review.NewEventBackend(platform, owner, repo, opts.pr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return emitSetupError(envelope, err)
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	fmt.Fprintf(os.Stderr, "- Lifecycle %s repo: %s/%s #%d at head %s\n",
		platform, owner, repo, opts.pr, shortHead(opts.head))

	result, runErr := review.RunLifecycle(lifecycleConfig(opts, platform, owner, repo, backend, logf))
	emitLifecycleResult(result)

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", runErr)
		return 1
	}
	return lifecycleExitCode(result.Status)
}

// newLifecycleEnvelope is the result skeleton for failures that happen before
// the platform and repo are known, so every lifecycle invocation still emits
// one machine-readable object.
func newLifecycleEnvelope(opts watchOptions) review.LifecycleResult {
	return review.LifecycleResult{
		Version:   review.ResultVersion,
		PR:        opts.pr,
		Head:      opts.head,
		StatePath: opts.state,
		WaitFor:   opts.waitFor,
		Authors:   review.NormalizeAuthors(opts.authors),
		Events:    []review.CollectedEvent{},
		Responded: []string{},
	}
}

// lifecycleConfig assembles the round configuration from parsed flags.
func lifecycleConfig(opts watchOptions, platform review.Platform, owner, repo string, backend review.EventBackend, logf func(string, ...any)) review.LifecycleConfig {
	cfg := review.LifecycleConfig{
		Platform:       platform,
		Owner:          owner,
		Repo:           repo,
		PR:             opts.pr,
		Head:           opts.head,
		StatePath:      opts.state,
		Authors:        opts.authors,
		TrustedAuthors: opts.trusted,
		WaitFor:        opts.waitFor,
		Timeout:        opts.timeout,
		Poll:           opts.poll,
		Backend:        backend,
		Log:            logf,
	}
	if !opts.timeoutSet {
		// The documented lifecycle default is longer than the legacy one; an
		// explicit --timeout always wins on either path.
		cfg.Timeout = review.DefaultLifecycleTimeout
	}
	return cfg
}

// resolveTargetFromRemote selects the receiving repository from --repo while
// the platform still comes from the origin remote, so the flag cannot redirect
// a watch to another host.
func resolveTargetFromRemote(remote, override string) (string, string, review.Platform, error) {
	platform := review.DetectPlatform(remote)
	trimmed := strings.Trim(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return "", "", platform, fmt.Errorf("lifecycle mode requires --repo owner/repo (the receiving repository)")
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", platform, fmt.Errorf("--repo must be owner/repo, got %q", override)
	}
	return parts[0], parts[1], platform, nil
}

// lifecycleExitCode maps a lifecycle status onto its documented exit code.
func lifecycleExitCode(status string) int {
	switch status {
	case review.StatusReady:
		return 0
	case review.StatusTimeout:
		return exitTimeout
	case review.StatusHeadChanged:
		return exitHeadChanged
	case review.StatusClosed:
		return exitPRClosed
	default:
		return 1
	}
}

// emitSetupError prints a complete error envelope for a failure that happened
// before the lifecycle round could start.
func emitSetupError(result review.LifecycleResult, err error) int {
	result.Status = review.StatusError
	result.Error = err.Error()
	emitLifecycleResult(result)
	return 1
}

// emitLifecycleResult writes exactly one JSON object to stdout, so a caller can
// parse it regardless of progress logs on stderr.
func emitLifecycleResult(result review.LifecycleResult) {
	if result.Events == nil {
		result.Events = []review.CollectedEvent{}
	}
	if result.Responded == nil {
		result.Responded = []string{}
	}
	if result.Outstanding == nil {
		result.Outstanding = []string{}
	}
	if result.Authors == nil {
		result.Authors = []string{}
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not encode lifecycle result: %v\n", err)
		return
	}
	fmt.Println(string(payload))
}

// shortHead abbreviates a SHA for progress logs.
func shortHead(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
