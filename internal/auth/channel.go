package auth

import "strings"

func (s *authService) IsChannelAuthorized(claims *Claims, channel string) bool {
	if claims == nil {
		return false
	}
	for _, pattern := range claims.Channels {
		switch {
		case pattern == "*":
			return true
		case strings.HasSuffix(pattern, ".*"):
			prefix := strings.TrimSuffix(pattern, ".*")
			if strings.HasPrefix(channel, prefix+".") {
				return true
			}
		default:
			if pattern == channel {
				return true
			}
		}
	}
	return false
}
