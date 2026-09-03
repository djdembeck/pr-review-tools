package review

import (
	"encoding/json"
)

// SelfLogin returns the login of the authenticated user for the given
// platform. GitHub uses the gh CLI; Forgejo uses the /user API endpoint.
func SelfLogin(platform Platform) (string, error) {
	switch platform {
	case PlatformForgejo:
		raw, err := forgejoGet("user")
		if err != nil {
			return "", err
		}
		var user struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal([]byte(raw), &user); err != nil {
			return "", err
		}
		return user.Login, nil
	default:
		out, err := runGh([]string{"api", "user", "--jq", ".login"})
		if err != nil {
			return "", err
		}
		return out, nil
	}
}
