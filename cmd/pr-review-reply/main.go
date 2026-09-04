// Command pr-review-reply posts feedback to PR review threads on GitHub or
// Forgejo. It supports Mira-style reject/acknowledge templates, raw custom
// replies, batch replies, and conversation resolution (GitHub only; Forgejo
// instances without a resolve API fail explicitly).
//
// Usage:
//
//	pr-review-reply <pr-number> --reject <comment-id> --reason "..."
//	pr-review-reply <pr-number> --acknowledge | --ack <comment-id> [--commit abc123]
//	pr-review-reply <pr-number> --reply <comment-id> --body "..." [--and-resolve]
//	pr-review-reply <pr-number> --batch-reply <file.json>
//	pr-review-reply <pr-number> --batch-reject <file.json>
//	pr-review-reply <pr-number> --batch-acknowledge | --batch-ack <file.json>
//	pr-review-reply <pr-number> --resolve <comment-id>
//	pr-review-reply <pr-number> --detect-bot
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

type action string

const (
	actionReject           action = "reject"
	actionAcknowledge      action = "acknowledge"
	actionReply            action = "reply"
	actionBatchReply       action = "batch-reply"
	actionBatchReject      action = "batch-reject"
	actionBatchAcknowledge action = "batch-acknowledge"
	actionResolve          action = "resolve"
	actionDetectBot        action = "detect-bot"
)

type args struct {
	prNumber      int
	act           action
	commentID     string
	reason        string
	note          string
	body          string
	batchFile     string
	resolveTarget string
	andResolve    bool
	commitHash    string
	botName       string
	dryRun        bool
	formatJSON    bool
}

func isHelp(s string) bool {
	return s == "--help" || s == "-h"
}

type rejectEntry struct {
	ID      string `json:"id"`
	Reason  string `json:"reason"`
	Resolve bool   `json:"resolve"`
}

type acknowledgeEntry struct {
	ID      string `json:"id"`
	Note    string `json:"note"`
	Resolve bool   `json:"resolve"`
}

// replyEntry is one entry of a --batch-reply file: a raw reply body and an
// optional resolve request.
type replyEntry struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	Resolve bool   `json:"resolve"`
}

const usageText = `pr-review-reply — Post feedback replies to PR review threads

Usage:
  pr-review-reply <pr-number> --reply <comment-id> --body "..." [--and-resolve]
  pr-review-reply <pr-number> --batch-reply <file.json>
  pr-review-reply <pr-number> --reject <comment-id> --reason "..." [--and-resolve]
  pr-review-reply <pr-number> --acknowledge | --ack <comment-id> [--commit abc123] [--and-resolve]
  pr-review-reply <pr-number> --batch-reject <file.json>
  pr-review-reply <pr-number> --batch-acknowledge | --batch-ack <file.json>
  pr-review-reply <pr-number> --resolve <comment-id>
  pr-review-reply <pr-number> --detect-bot

Options:
  --body <text>       Raw reply body (required by --reply)
  --and-resolve       After a successful reply, resolve the thread (GitHub only)
  --resolve <id>      Resolve the thread containing <id> without replying (GitHub only)
  --bot-name <name>   Override bot name (default: auto-detect)
  --commit <hash>     Include commit hash in acknowledgment
  --note <text>       Append a note to the acknowledgment
  --dry-run           Print actions without posting
  --format json       Output results as JSON
  --help              Show this help

Batch files:
  --batch-reply entries: {"id": "...", "body": "...", "resolve": true}
  --batch-reject / --batch-acknowledge entries additionally accept "resolve": true.`

func printUsage() {
	fmt.Fprintln(os.Stderr, usageText)
}

func parseArgs(argv []string) args {
	a, err, showHelp := parseArgsValue(argv)
	if showHelp {
		printUsage()
		os.Exit(0)
	}
	if err != "" {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return a
}

// parseArgsValue parses CLI arguments without touching os.Exit so it can be
// unit-tested. It returns the parsed args, a usage error ("" on success), and
// whether the user requested help.
func parseArgsValue(argv []string) (args, string, bool) {
	a := args{}
	argsList := argv[1:]
	if len(argsList) == 0 {
		return a, "Usage error: missing PR number\n" + usageText + "\n", false
	}
	// --help / -h may appear as the first arg without a PR number.
	if isHelp(argsList[0]) {
		return a, "", true
	}

	prNumber, err := strconv.Atoi(argsList[0])
	if err != nil || prNumber <= 0 {
		return a, fmt.Sprintf("Invalid PR number: %s\n", argsList[0]), false
	}
	a.prNumber = prNumber

	// primaryAction enforces one action per invocation: --and-resolve is a
	// modifier, but every other action flag selects the sole action.
	primaryAction := func(flag action, flagName string) error {
		if a.act != "" {
			return fmt.Errorf("error: one action per invocation; both --%s and %s were given", a.act, flagName)
		}
		a.act = flag
		return nil
	}

	for i := 1; i < len(argsList); i++ {
		arg := argsList[i]
		// Primary action flag that takes a value. The next token must exist
		// and must not be flag-shaped, otherwise a bare flag like --dry-run
		// would be silently consumed as the value.
		if flag, flagName, ok := primaryActionFlag(arg); ok {
			if err := primaryAction(flag, flagName); err != nil {
				return a, err.Error(), false
			}
			value, valueErr := optionValue(argsList, i, flagName)
			if valueErr != "" {
				return a, valueErr, false
			}
			i++
			switch flag {
			case actionResolve:
				a.resolveTarget = value
			case actionBatchReply, actionBatchReject, actionBatchAcknowledge:
				a.batchFile = value
			default:
				a.commentID = value
			}
			continue
		}
		switch arg {
		case "--and-resolve":
			a.andResolve = true
		case "--reason", "--body", "--commit", "--note", "--bot-name":
			value, valueErr := optionValue(argsList, i, arg)
			if valueErr != "" {
				return a, valueErr, false
			}
			i++
			switch arg {
			case "--reason":
				a.reason = value
			case "--body":
				a.body = value
			case "--commit":
				a.commitHash = value
			case "--note":
				a.note = value
			case "--bot-name":
				a.botName = value
			}
		case "--detect-bot":
			if err := primaryAction(actionDetectBot, "--detect-bot"); err != nil {
				return a, err.Error(), false
			}
		case "--dry-run":
			a.dryRun = true
		case "--format":
			value, valueErr := optionValue(argsList, i, arg)
			if valueErr != "" {
				return a, valueErr, false
			}
			i++
			if value == "json" {
				a.formatJSON = true
			}
		case "--help", "-h":
			return a, "", true
		default:
			return a, fmt.Sprintf("Unknown argument: %s\n%s\n", arg, usageText), false
		}
	}

	if a.act == "" {
		return a, "Error: must specify --reply, --resolve, --reject, --acknowledge, --batch-reply, --batch-reject, --batch-acknowledge, or --detect-bot\n" + usageText + "\n", false
	}
	if (a.act == actionReject || a.act == actionAcknowledge || a.act == actionReply) && a.commentID == "" {
		return a, fmt.Sprintf("Error: --%s requires a comment ID\n", a.act), false
	}
	if a.act == actionReject && a.reason == "" {
		return a, "Error: --reject requires --reason\n", false
	}
	if a.act == actionReply && a.body == "" {
		return a, "Error: --reply requires --body\n", false
	}
	if a.act == actionResolve && a.resolveTarget == "" {
		return a, "Error: --resolve requires a comment ID\n", false
	}
	if a.act == actionBatchReply && a.batchFile == "" {
		return a, "Error: --batch-reply requires a file path\n", false
	}
	if (a.act == actionBatchReject || a.act == actionBatchAcknowledge) && a.batchFile == "" {
		return a, fmt.Sprintf("Error: --%s requires a file path\n", a.act), false
	}
	return a, "", false
}

// primaryActionFlag reports whether arg is a primary action flag and which
// action it maps to. Aliases resolve to their canonical flag name for error
// messages.
func primaryActionFlag(arg string) (action, string, bool) {
	switch arg {
	case "--reject":
		return actionReject, "--reject", true
	case "--acknowledge", "--ack":
		return actionAcknowledge, "--acknowledge", true
	case "--reply":
		return actionReply, "--reply", true
	case "--batch-reply":
		return actionBatchReply, "--batch-reply", true
	case "--batch-reject":
		return actionBatchReject, "--batch-reject", true
	case "--batch-acknowledge", "--batch-ack":
		return actionBatchAcknowledge, "--batch-acknowledge", true
	case "--resolve":
		return actionResolve, "--resolve", true
	}
	return "", "", false
}

// optionValue returns the value that must follow option flag at index i in
// args. It errors when the value is missing at the end of args or when the
// next token is flag-shaped, so a bare flag is never silently consumed as a
// value.
func optionValue(args []string, i int, flag string) (string, string) {
	if i+1 >= len(args) {
		return "", fmt.Sprintf("Error: %s requires a value\n", flag)
	}
	next := args[i+1]
	if isFlagShaped(next) {
		return "", fmt.Sprintf("Error: %s requires a value; %q looks like a flag\n", flag, next)
	}
	return next, ""
}

// isFlagShaped reports whether s would be parsed as a flag.
func isFlagShaped(s string) bool {
	return len(s) > 1 && strings.HasPrefix(s, "-")
}

func readBatchFile(path string, out interface{}) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading batch file: %v\n", err)
		os.Exit(1)
	}
	var arr json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		fmt.Fprintf(os.Stderr, "Error: batch file must contain a JSON array: %s\n", path)
		os.Exit(1)
	}
	// arr is a raw JSON value; check it's an array by inspecting the first
	// non-whitespace byte.
	trimmed := strings.TrimSpace(string(arr))
	if !strings.HasPrefix(trimmed, "[") {
		fmt.Fprintf(os.Stderr, "Error: batch file must contain a JSON array: %s\n", path)
		os.Exit(1)
	}
	if err := json.Unmarshal(data, out); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing batch file: %v\n", err)
		os.Exit(1)
	}
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func main() {
	opts := parseArgs(os.Args)

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

	fmt.Fprintf(os.Stderr, "- Platform: %s, Repo: %s/%s, PR: %d\n", platform, owner, repo, opts.prNumber)

	// Detect or use provided bot name.
	botName := opts.botName
	if botName == "" {
		switch platform {
		case review.PlatformGitHub:
			botName, _ = review.DetectGitHubBotName(owner, repo, opts.prNumber)
		case review.PlatformForgejo:
			botName, _ = review.DetectForgejoBotName(owner, repo, opts.prNumber)
		}
		if opts.act == actionDetectBot {
			fmt.Println(botName)
			return
		}
		fmt.Fprintf(os.Stderr, "- Detected bot name: %s\n", botName)
	}

	if opts.act == actionDetectBot {
		fmt.Println(botName)
		return
	}

	postReply := func(cid, body string) review.ReplyResult {
		if platform == review.PlatformGitHub {
			res, _ := review.PostGitHubReply(owner, repo, opts.prNumber, cid, body, opts.dryRun)
			if opts.dryRun {
				fmt.Fprintf(os.Stderr, "  [dry-run] Would reply to comment %s: %s...\n", cid, truncate(body, 80))
			}
			return res
		}
		res, _ := review.PostForgejoReply(owner, repo, opts.prNumber, cid, body, opts.dryRun)
		if opts.dryRun {
			fmt.Fprintf(os.Stderr, "  [dry-run] Would reply to comment %s: %s...\n", cid, truncate(body, 80))
		}
		return res
	}

	// resolveRequest carries one resolve request. commentID is used for
	// thread lookup (GitHub) or the fixed failure (Forgejo).
	type resolveRequest struct {
		commentID string
	}

	var preFetched []review.RawComment
	var preFetchErr error
	needPrefetch := platform == review.PlatformGitHub && !opts.dryRun &&
		(opts.act == actionResolve || opts.andResolve || opts.act == actionBatchReply)
	// Batch actions decide resolve needs per entry; always allow the resolver
	// to fall back to a lazily-issued fetch if a batch requests resolution.
	if needPrefetch {
		preFetched, preFetchErr = review.FetchGitHubComments(owner, repo, opts.prNumber)
	}

	// resolveComment performs one resolve request against the (possibly
	// pre-fetched) comment list and returns the result appended by callers.
	// Single-goroutine invariant: preFetched/preFetchErr are shared mutable
	// state guarded only by this CLI invoking runResolve strictly sequentially
	// on one goroutine; do not parallelize the batch loops without sync.
	runResolve := func(rr resolveRequest) review.ReplyResult {
		res := review.ReplyResult{CommentID: rr.commentID, Action: "resolve"}
		if opts.dryRun {
			fmt.Fprintf(os.Stderr, "  [dry-run] Would resolve thread for comment %s\n", rr.commentID)
			res.Success = true
			return res
		}
		if platform == review.PlatformForgejo {
			return review.ResolveForgejoThread(owner, repo, opts.prNumber, rr.commentID)
		}
		if preFetchErr != nil {
			res.Success = false
			res.Error = preFetchErr.Error()
			return res
		}
		if preFetched == nil {
			preFetched, preFetchErr = review.FetchGitHubComments(owner, repo, opts.prNumber)
			if preFetchErr != nil {
				res.Success = false
				res.Error = preFetchErr.Error()
				return res
			}
		}
		threadID, ok := review.FindThreadID(preFetched, rr.commentID)
		if !ok {
			res.Success = false
			res.Error = fmt.Sprintf("no review thread contains comment %s on PR %d", rr.commentID, opts.prNumber)
			return res
		}
		gRes, _ := review.ResolveGitHubThread(owner, repo, opts.prNumber, threadID, false)
		gRes.CommentID = rr.commentID
		gRes.Action = "resolve"
		return gRes
	}

	var results []review.ReplyResult

	switch opts.act {
	case actionReply:
		fmt.Fprintf(os.Stderr, "- Replying to comment %s: %s...\n", opts.commentID, truncate(opts.body, 80))
		res := postReply(opts.commentID, opts.body)
		results = append(results, review.ReplyResult{
			CommentID: opts.commentID,
			Action:    "reply",
			Body:      opts.body,
			Success:   res.Success,
			Error:     res.Error,
			ReplyURL:  res.ReplyURL,
		})
		if res.Success && opts.andResolve {
			results = append(results, runResolve(resolveRequest{commentID: opts.commentID}))
		}

	case actionResolve:
		results = append(results, runResolve(resolveRequest{commentID: opts.resolveTarget}))

	case actionReject:
		body := review.BuildRejectBody(botName, opts.reason, opts.commitHash)
		fmt.Fprintf(os.Stderr, "- Rejecting comment %s: %s...\n", opts.commentID, truncate(opts.reason, 80))
		res := postReply(opts.commentID, body)
		results = append(results, review.ReplyResult{
			CommentID: opts.commentID,
			Action:    "reject",
			Body:      body,
			Success:   res.Success,
			Error:     res.Error,
			ReplyURL:  res.ReplyURL,
		})
		if res.Success && opts.andResolve {
			results = append(results, runResolve(resolveRequest{commentID: opts.commentID}))
		}

	case actionAcknowledge:
		body := review.BuildAcknowledgeBody(opts.commitHash, opts.note)
		fmt.Fprintf(os.Stderr, "- Acknowledging comment %s\n", opts.commentID)
		res := postReply(opts.commentID, body)
		results = append(results, review.ReplyResult{
			CommentID: opts.commentID,
			Action:    "acknowledge",
			Body:      body,
			Success:   res.Success,
			Error:     res.Error,
			ReplyURL:  res.ReplyURL,
		})
		if res.Success && opts.andResolve {
			results = append(results, runResolve(resolveRequest{commentID: opts.commentID}))
		}

	case actionBatchReply:
		var entries []replyEntry
		readBatchFile(opts.batchFile, &entries)
		fmt.Fprintf(os.Stderr, "- Batch reply: %d comments from %s\n", len(entries), opts.batchFile)
		for _, entry := range entries {
			if entry.ID == "" || entry.Body == "" {
				results = append(results, review.ReplyResult{
					CommentID: review.FallbackID(entry.ID),
					Action:    "reply",
					Body:      "",
					Success:   false,
					Error:     "Missing id or body in batch entry",
				})
				continue
			}
			fmt.Fprintf(os.Stderr, "  - Replying to %s: %s...\n", entry.ID, truncate(entry.Body, 60))
			res := postReply(entry.ID, entry.Body)
			results = append(results, review.ReplyResult{
				CommentID: entry.ID,
				Action:    "reply",
				Body:      entry.Body,
				Success:   res.Success,
				Error:     res.Error,
				ReplyURL:  res.ReplyURL,
			})
			if res.Success && entry.Resolve {
				results = append(results, runResolve(resolveRequest{commentID: entry.ID}))
			}
		}

	case actionBatchReject:
		var entries []rejectEntry
		readBatchFile(opts.batchFile, &entries)
		fmt.Fprintf(os.Stderr, "- Batch reject: %d comments from %s\n", len(entries), opts.batchFile)
		for _, entry := range entries {
			if entry.ID == "" || entry.Reason == "" {
				results = append(results, review.ReplyResult{
					CommentID: review.FallbackID(entry.ID),
					Action:    "reject",
					Body:      "",
					Success:   false,
					Error:     "Missing id or reason in batch entry",
				})
				continue
			}
			body := review.BuildRejectBody(botName, entry.Reason, opts.commitHash)
			fmt.Fprintf(os.Stderr, "  - Rejecting %s: %s...\n", entry.ID, truncate(entry.Reason, 60))
			res := postReply(entry.ID, body)
			results = append(results, review.ReplyResult{
				CommentID: entry.ID,
				Action:    "reject",
				Body:      body,
				Success:   res.Success,
				Error:     res.Error,
				ReplyURL:  res.ReplyURL,
			})
			if res.Success && entry.Resolve {
				results = append(results, runResolve(resolveRequest{commentID: entry.ID}))
			}
		}

	case actionBatchAcknowledge:
		var entries []acknowledgeEntry
		readBatchFile(opts.batchFile, &entries)
		fmt.Fprintf(os.Stderr, "- Batch acknowledge: %d comments from %s\n", len(entries), opts.batchFile)
		for _, entry := range entries {
			if entry.ID == "" {
				results = append(results, review.ReplyResult{
					CommentID: "?",
					Action:    "acknowledge",
					Body:      "",
					Success:   false,
					Error:     "Missing id in batch entry",
				})
				continue
			}
			body := review.BuildAcknowledgeBody(opts.commitHash, entry.Note)
			fmt.Fprintf(os.Stderr, "  - Acknowledging %s\n", entry.ID)
			res := postReply(entry.ID, body)
			results = append(results, review.ReplyResult{
				CommentID: entry.ID,
				Action:    "acknowledge",
				Body:      body,
				Success:   res.Success,
				Error:     res.Error,
				ReplyURL:  res.ReplyURL,
			})
			if res.Success && entry.Resolve {
				results = append(results, runResolve(resolveRequest{commentID: entry.ID}))
			}
		}
	}

	succeeded := 0
	for _, r := range results {
		if r.Success {
			succeeded++
		}
	}
	failed := len(results) - succeeded

	if opts.formatJSON {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(strings.TrimRight(buf.String(), "\n"))
	} else {
		for _, r := range results {
			if r.Success {
				extra := ""
				if r.ReplyURL != "" {
					extra = " → " + r.ReplyURL
				}
				fmt.Fprintf(os.Stderr, "  ✓ %s %s%s\n", r.Action, r.CommentID, extra)
			} else {
				fmt.Fprintf(os.Stderr, "  ✗ %s %s: %s\n", r.Action, r.CommentID, r.Error)
			}
		}
		fmt.Fprintf(os.Stderr, "\nDone: %d succeeded, %d failed\n", succeeded, failed)
	}

	if failed > 0 {
		os.Exit(1)
	}
}
