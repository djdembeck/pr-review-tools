package review

import "encoding/json"

// Platform identifies the code hosting backend.
type Platform string

const (
	PlatformGitHub  Platform = "github"
	PlatformForgejo Platform = "forgejo"
)

// Severity classifies a Mira comment's importance.
type Severity string

const (
	SeverityBlocker    Severity = "blocker"
	SeverityWarning    Severity = "warning"
	SeveritySuggestion Severity = "suggestion"
	SeverityNitpick    Severity = "nitpick"
)

// RawComment is the normalized API response comment shape, shared by both
// platforms. Field tags mirror the TS RawComment interface (camelCase).
type RawComment struct {
	ID            string  `json:"id"`
	Body          string  `json:"body"`
	Path          *string `json:"path"` // null if missing
	Line          *int    `json:"line"` // null if missing
	StartLine     *int    `json:"startLine"`
	DiffHunk      *string `json:"diffHunk"`
	Author        string  `json:"author"`
	CreatedAt     *string `json:"createdAt"`
	IsResolved    bool    `json:"isResolved"`
	IsOutdated    bool    `json:"isOutdated"`
	ReplyToID     *string `json:"replyToId"` // null = root comment
	ThreadID      *string `json:"threadId"`  // null = not a threaded platform (e.g. Forgejo)
	ThreadReplies int     `json:"threadReplies"`
}

// ParsedComment is the output type matching the TS ParsedMiraComment. No
// omitempty: every field is always present in the output, mirroring the TS
// JSON.stringify behavior.
type ParsedComment struct {
	ID            string   `json:"id"`
	File          string   `json:"file"`
	LineStart     int      `json:"lineStart"`
	LineEnd       int      `json:"lineEnd"`
	Category      string   `json:"category"`
	Severity      Severity `json:"severity"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	Author        string   `json:"author"`      // author login of the root comment
	IsMira        bool     `json:"isMira"`      // true iff IsMiraComment(Author)
	IsTrusted     bool     `json:"isTrusted"`   // true iff Author is a trusted prompt source (bot-suffix login or --trusted-authors entry)
	Suggestion    *string  `json:"suggestion"`  // null when absent
	AgentPrompt   *string  `json:"agentPrompt"` // null when absent or author untrusted
	DiffHunk      *string  `json:"diffHunk"`    // null when absent
	IsResolved    bool     `json:"isResolved"`
	IsOutdated    bool     `json:"isOutdated"`
	CreatedAt     *string  `json:"createdAt"` // null when absent
	ThreadID      *string  `json:"threadId"`  // null when absent (non-threaded platform)
	ThreadReplies int      `json:"threadReplies"`
}

// ReplyResult mirrors the TS ReplyResult. Error/ReplyURL use omitempty because
// TS JSON.stringify omits undefined fields.
type ReplyResult struct {
	CommentID string `json:"commentId"`
	Action    string `json:"action"`
	Body      string `json:"body"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	ReplyURL  string `json:"replyUrl,omitempty"`
}

// ─── API response deserialization structs ───────────────────────────────────

// ghAuthor matches the GraphQL author object.
type ghAuthor struct {
	Login string `json:"login"`
}

// ghReplyTo is null for root comments, otherwise holds a databaseId.
type ghReplyTo struct {
	DatabaseID json.Number `json:"databaseId"`
}

// ghCommentNode is a single comment inside a review thread.
type ghCommentNode struct {
	DatabaseID        json.Number `json:"databaseId"`
	Body              string      `json:"body"`
	Author            *ghAuthor   `json:"author"`
	Path              *string     `json:"path"`
	Line              *int        `json:"line"`
	OriginalLine      *int        `json:"originalLine"`
	StartLine         *int        `json:"startLine"`
	OriginalStartLine *int        `json:"originalStartLine"`
	DiffHunk          *string     `json:"diffHunk"`
	CreatedAt         *string     `json:"createdAt"`
	ReplyTo           *ghReplyTo  `json:"replyTo"`
}

// ghPageInfo is the pagination metadata for a GraphQL connection.
type ghPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// ghComments wraps the GraphQL comments connection.
type ghComments struct {
	Nodes    []ghCommentNode `json:"nodes"`
	PageInfo ghPageInfo      `json:"pageInfo"`
}

// ghThread is a single review thread.
type ghThread struct {
	ID         string     `json:"id"`
	IsResolved bool       `json:"isResolved"`
	IsOutdated bool       `json:"isOutdated"`
	Comments   ghComments `json:"comments"`
}

// ghReviewThreads wraps the reviewThreads connection.
type ghReviewThreads struct {
	Nodes    []ghThread `json:"nodes"`
	PageInfo ghPageInfo `json:"pageInfo"`
}

// ghPullRequest is the pullRequest object.
type ghPullRequest struct {
	ReviewThreads ghReviewThreads `json:"reviewThreads"`
}

// ghRepository is the repository object.
type ghRepository struct {
	PullRequest *ghPullRequest `json:"pullRequest"`
}

// ghData is the GraphQL data envelope.
type ghData struct {
	Repository *ghRepository `json:"repository"`
}

// graphQLResponse is the top-level GitHub GraphQL response.
type graphQLResponse struct {
	Data   *ghData        `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// graphQLError is a single GraphQL error entry.
type graphQLError struct {
	Message string `json:"message"`
}

// ─── Forgejo API deserialization structs ────────────────────────────────────

// forgejoUser is the user object on Forgejo API responses.
type forgejoUser struct {
	Login string `json:"login"`
}

// forgejoComment is a single Forgejo review comment.
type forgejoComment struct {
	ID               json.Number  `json:"id"`
	Body             string       `json:"body"`
	User             forgejoUser  `json:"user"`
	DiffHunk         *string      `json:"diff_hunk"`
	Diff             *string      `json:"diff"`
	InReplyToID      *int64       `json:"in_reply_to_id"`
	Path             *string      `json:"path"`
	Line             *int         `json:"line"`
	StartLine        *int         `json:"start_line"`
	Position         int          `json:"position"`          // new-file line (LineNum); 0 if none
	OriginalPosition int          `json:"original_position"` // old-file line (OldLineNum); 0 if none
	CreatedAt        *string      `json:"created_at"`
	Resolver         *forgejoUser `json:"resolver"` // non-null = conversation resolved
}

// forgejoReview is a single Forgejo review (holds an id + author).
type forgejoReview struct {
	ID   json.Number `json:"id"`
	User forgejoUser `json:"user"`
}

// errResp is the Forgejo error response envelope.
type errResp struct {
	Message string `json:"message"`
}
