package review

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// GetGitRemote returns the trimmed "origin" remote URL of the current git
// repository.
func GetGitRemote() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		// Fall back to capturing stderr for a useful message.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git remote get-url failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git remote get-url failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GitCommitTime returns the commit timestamp (ISO 8601 / RFC3339) of the given
// git ref.
func GitCommitTime(ref string) (time.Time, error) {
	out, err := exec.Command("git", "log", "-1", "--format=%cI", ref).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return time.Time{}, fmt.Errorf("git log for %q failed: %s", ref, strings.TrimSpace(string(ee.Stderr)))
		}
		return time.Time{}, fmt.Errorf("git log for %q failed: %w", ref, err)
	}
	commitTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}, fmt.Errorf("could not parse commit time for %q: %w", ref, err)
	}
	return commitTime, nil
}

// ParseSinceArg resolves the --since flag value: either an RFC3339 timestamp
// or a git ref whose commit time is used.
func ParseSinceArg(s string) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, nil
	}
	if ts, err := GitCommitTime(s); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q: not an RFC3339 timestamp or a git ref", s)
}

// DetectPlatform returns the platform inferred from the remote URL. GitHub is
// the default for unknown hosts.
func DetectPlatform(remote string) Platform {
	switch {
	case strings.Contains(remote, "github.com"):
		return PlatformGitHub
	case strings.Contains(remote, "git.theiahd.nl"):
		return PlatformForgejo
	default:
		return PlatformGitHub
	}
}

var (
	reGitAt       = regexp.MustCompile(`^git@[^:]+:`)
	reSSHPort     = regexp.MustCompile(`^ssh://git@[^:]+:\d+/`)
	reHTTPSPrefix = regexp.MustCompile(`^https?://[^/]+/`)
	reGitSuffix   = regexp.MustCompile(`\.git$`)
)

// ParseRemoteRepo extracts the owner and repo from a git remote URL. Supports
// git@host:owner/repo.git, ssh://git@host:port/owner/repo.git, and
// https://host/owner/repo.git forms. The repo portion may contain slashes.
func ParseRemoteRepo(remote string) (owner, repo string, err error) {
	cleaned := remote
	cleaned = reGitAt.ReplaceAllString(cleaned, "")
	cleaned = reSSHPort.ReplaceAllString(cleaned, "")
	cleaned = reHTTPSPrefix.ReplaceAllString(cleaned, "")
	cleaned = reGitSuffix.ReplaceAllString(cleaned, "")

	parts := strings.Split(cleaned, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("could not parse owner/repo from remote: %s", remote)
	}
	return parts[0], strings.Join(parts[1:], "/"), nil
}
