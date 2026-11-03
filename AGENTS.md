# Agents

## Stack and scope

Go CLI pair (`pr-review-parser`, `pr-review-replyer`) for parsing PR review comments into a stable JSON schema and replying on GitHub/Forgejo. Output changes are reviewed against the README "Output schema" section; platform behavior against "Forgejo limitations" (resolve unsupported, no comment threading, `threadId` always `null`).

## Trust boundary

`internal/review/parse.go` `IsTrustedAuthor` is the AgentPrompt trust boundary: an author is trusted only when its login is an exact, case-insensitive match (after trim) against a `--trusted-authors` entry. There is no suffix-based trust and no hardcoded allowlist. `agentPrompt` is emitted for trusted authors only — untrusted review text must never be promoted to instructions, even when the body contains an agent-prompt block.
