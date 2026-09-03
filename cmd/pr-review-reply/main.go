// Command pr-review-reply posts feedback to PR review threads on GitHub or
// Forgejo. It supports Mira-style reject/acknowledge templates, raw custom
// replies, batch replies, and conversation resolution (GitHub only; Forgejo
// instances without a resolve API fail explicitly).
//
// Usage:
//
//	pr-review-reply <pr-number> --reject <comment-id> --reason "..."
//	pr-review-reply <pr-number> --acknowledge <comment-id> [--commit abc123]
//	pr-review-reply <pr-number> --reply <comment-id> --body "..." [--and-resolve]
//	pr-review-reply <pr-number> --batch-reply <file.json>
//	pr-review-reply <pr-number> --batch-reject <file.json>
//	pr-review-reply <pr-number> --batch-acknowledge <file.json>
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

func printUsage() {
	fmt.Fprintln(os.Stderr, `pr-review-reply — Post feedback replies to PR review threads

Usage:
  pr-review-reply <pr-number> --reply <comment-id> --body "..." [--and-resolve]
  pr-review-reply <pr-number> --batch-reply <file.json>
  pr-review-reply <pr-number> --reject <comment-id> --reason "..." [--and-resolve]
  pr-review-reply <pr-number> --acknowledge <comment-id> [--commit abc123] [--and-resolve]
  pr-review-reply <pr-number> --batch-reject <file.json>
  pr-review-reply <pr-number> --batch-acknowledge <file.json>
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
  --batch-reject / --batch-acknowledge entries additionally accept "resolve": true.`)
}

func parseArgs(argv []string) args {
	a := args{}
	argsList := argv[1:]
	if len(argsList) == 0 {
		printUsage()
		os.Exit(1)
	}
	// --help / -h may appear as the first arg without a PR number.
	if isHelp(argsList[0]) {
		printUsage()
		os.Exit(0)
	}

	prNumber, err := strconv.Atoi(argsList[0])
	if err != nil || prNumber <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid PR number: %s\n", argsList[0])
		os.Exit(1)
	}
	a.prNumber = prNumber

	for i := 1; i < len(argsList); i++ {
		arg := argsList[i]
		switch arg {
		case "--reject":
			a.act = actionReject
			i++
			if i < len(argsList) {
				a.commentID = argsList[i]
			}
		case "--acknowledge", "--ack":
			a.act = actionAcknowledge
			i++
			if i < len(argsList) {
				a.commentID = argsList[i]
			}
		case "--reply":
			a.act = actionReply
			i++
			if i < len(argsList) {
				a.commentID = argsList[i]
			}
		case "--batch-reply":
			a.act = actionBatchReply
			i++
			if i < len(argsList) {
				a.batchFile = argsList[i]
			}
		case "--batch-reject":
			a.act = actionBatchReject
			i++
			if i < len(argsList) {
				a.batchFile = argsList[i]
			}
		case "--batch-acknowledge", "--batch-ack":
			a.act = actionBatchAcknowledge
			i++
			if i < len(argsList) {
				a.batchFile = argsList[i]
			}
		case "--resolve":
			a.act = actionResolve
			i++
			if i < len(argsList) {
				a.resolveTarget = argsList[i]
			}
		case "--and-resolve":
			a.andResolve = true
		case "--reason":
			i++
			if i < len(argsList) {
				a.reason = argsList[i]
			}
		case "--body":
			i++
			if i < len(argsList) {
				a.body = argsList[i]
			}
		case "--commit":
			i++
			if i < len(argsList) {
				a.commitHash = argsList[i]
			}
		case "--note":
			i++
			if i < len(argsList) {
				a.note = argsList[i]
			}
		case "--bot-name":
			i++
			if i < len(argsList) {
				a.botName = argsList[i]
			}
		case "--detect-bot":
			a.act = actionDetectBot
		case "--dry-run":
			a.dryRun = true
		case "--format":
			i++
			if i < len(argsList) && argsList[i] == "json" {
				a.formatJSON = true
			}
		case "--help", "-h":
			printUsage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Unknown argument: %s\n", arg)
			printUsage()
			os.Exit(1)
		}
	}

	if a.act == "" {
		fmt.Fprintln(os.Stderr, "Error: must specify --reply, --resolve, --reject, --acknowledge, --batch-reply, --batch-reject, --batch-acknowledge, or --detect-bot")
		printUsage()
		os.Exit(1)
	}
	if (a.act == actionReject || a.act == actionAcknowledge || a.act == actionReply) && a.commentID == "" {
		fmt.Fprintf(os.Stderr, "Error: --%s requires a comment ID\n", a.act)
		os.Exit(1)
	}
	if a.act == actionReject && a.reason == "" {
		fmt.Fprintln(os.Stderr, "Error: --reject requires --reason")
		os.Exit(1)
	}
	if a.act == actionReply && a.body == "" {
		fmt.Fprintln(os.Stderr, "Error: --reply requires --body")
		os.Exit(1)
	}
	if a.act == actionResolve && a.resolveTarget == "" {
		fmt.Fprintln(os.Stderr, "Error: --resolve requires a comment ID")
		os.Exit(1)
	}
	if a.act == actionBatchReply && a.batchFile == "" {
		fmt.Fprintln(os.Stderr, "Error: --batch-reply requires a file path")
		os.Exit(1)
	}
	if (a.act == actionBatchReject || a.act == actionBatchAcknowledge) && a.batchFile == "" {
		fmt.Fprintf(os.Stderr, "Error: --%s requires a file path\n", a.act)
		os.Exit(1)
	}
	return a
}

// readBatchFile reads a JSON array from path. It accepts raw arrays of
// `{id,reason}` or `{id,note}` objects.
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
