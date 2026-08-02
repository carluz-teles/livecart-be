package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// signaturePrefix is what Meta puts in front of the digest in X-Hub-Signature-256.
const signaturePrefix = "sha256="

// signatureOutcome explains why a webhook payload was (not) accepted, so the
// observation-mode logs can be read without guessing.
type signatureOutcome string

const (
	// signatureValid — the digest matched. The only outcome that stays valid
	// once enforcement is turned on.
	signatureValid signatureOutcome = "valid"
	// signatureMismatch — a signature came in and did not match the body. This
	// is the one that means "someone is forging, or the secret is wrong".
	signatureMismatch signatureOutcome = "mismatch"
	// signatureMissing — Meta always signs, so an unsigned request is either a
	// forgery or our own tooling (emulator / send-comment.sh / E2E skill).
	signatureMissing signatureOutcome = "missing_header"
	// signatureMalformed — header present but not "sha256=<hex>".
	signatureMalformed signatureOutcome = "malformed_header"
	// signatureUnconfigured — no INSTAGRAM_APP_SECRET in this environment. Dev
	// and staging run like this; enforcement must never reject on this outcome
	// or a missing env var would take the whole ingestion path down.
	signatureUnconfigured signatureOutcome = "secret_not_configured"
)

// ok reports whether the outcome should let the payload through when
// enforcement is on.
func (o signatureOutcome) ok() bool {
	return o == signatureValid || o == signatureUnconfigured
}

// verifyInstagramSignature checks the raw request body against the
// X-Hub-Signature-256 header using HMAC-SHA256 keyed with the app secret.
//
// The comparison is constant-time: a byte-by-byte compare would leak the
// expected digest through timing, which is enough to forge a signature.
func verifyInstagramSignature(body []byte, header, secret string) signatureOutcome {
	if secret == "" {
		return signatureUnconfigured
	}
	if header == "" {
		return signatureMissing
	}

	if !strings.HasPrefix(header, signaturePrefix) {
		return signatureMalformed
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil || len(got) != sha256.Size {
		return signatureMalformed
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return signatureMismatch
	}
	return signatureValid
}
