package auth

import "crypto/subtle"

func (s *authService) ValidateAPIKey(key string) bool {
	target := []byte(key)
	matched := 0
	for _, k := range s.apiKeys {
		matched |= subtle.ConstantTimeCompare(k, target)
	}
	return matched == 1
}
