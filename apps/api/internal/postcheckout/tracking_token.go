package postcheckout

import (
	"crypto/rand"
	"encoding/base64"
)

// trackingTokenBytes controls the URL-safe random token length. 24 bytes →
// 32 base64-url-encoded characters with no padding. Effectively unguessable
// (192 bits) so the public order URL alone is the security boundary; we don't
// need a second per-customer secret.
const trackingTokenBytes = 24

func generateTrackingToken() (string, error) {
	var b [trackingTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
