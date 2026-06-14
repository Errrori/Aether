package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VerifySignature computes HMAC-SHA256(body, secret) and compares it
// to the provided signature using constant-time comparison.
// The signature parameter may include an optional "sha256=" prefix
// (compatible with GitHub and other webhook providers).
func VerifySignature(secret []byte, body []byte, signature string) bool {
	sig := signature
	if strings.HasPrefix(sig, "sha256=") {
		sig = sig[len("sha256="):]
	}

	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	actual := mac.Sum(nil)

	return hmac.Equal(actual, expected)
}
