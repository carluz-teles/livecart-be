package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(t *testing.T, body []byte, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyInstagramSignature(t *testing.T) {
	const secret = "app-secret-de-teste"
	body := []byte(`{"object":"instagram","entry":[{"id":"17841400000000000"}]}`)
	valid := sign(t, body, secret)

	cases := []struct {
		name   string
		body   []byte
		header string
		secret string
		want   signatureOutcome
	}{
		{"assinatura correta", body, valid, secret, signatureValid},
		{"corpo adulterado depois de assinar", append(body, ' '), valid, secret, signatureMismatch},
		{"assinada com outro segredo", body, sign(t, body, "outro-segredo"), secret, signatureMismatch},
		{"sem header", body, "", secret, signatureMissing},
		{"header sem o prefixo sha256=", body, hex.EncodeToString(make([]byte, sha256.Size)), secret, signatureMalformed},
		{"header com prefixo sha1", body, "sha1=" + hex.EncodeToString(make([]byte, sha256.Size)), secret, signatureMalformed},
		{"hex invalido", body, signaturePrefix + "nao-e-hex", secret, signatureMalformed},
		{"digest curto demais", body, signaturePrefix + "abcd", secret, signatureMalformed},
		// Dev, staging e o emulador rodam sem segredo. Isso NAO pode virar
		// rejeicao quando a aplicacao estiver ligada, senao um env var faltando
		// derruba a ingestao inteira.
		{"sem segredo configurado", body, valid, "", signatureUnconfigured},
		{"sem segredo e sem header", body, "", "", signatureUnconfigured},
		// O emulador manda a string literal "simulated" no header.
		{"header do emulador", body, "simulated", secret, signatureMalformed},
		{"corpo vazio assinado", []byte{}, sign(t, []byte{}, secret), secret, signatureValid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyInstagramSignature(tc.body, tc.header, tc.secret); got != tc.want {
				t.Errorf("verifyInstagramSignature = %q, quero %q", got, tc.want)
			}
		})
	}
}

// ok() decide quem passa quando a aplicacao esta ligada. Um outcome novo que
// caia no default errado abriria o endpoint de novo, entao o mapa e explicito.
func TestSignatureOutcomeOK(t *testing.T) {
	cases := map[signatureOutcome]bool{
		signatureValid:        true,
		signatureUnconfigured: true, // dev/staging sem segredo
		signatureMismatch:     false,
		signatureMissing:      false,
		signatureMalformed:    false,
	}
	for outcome, want := range cases {
		if got := outcome.ok(); got != want {
			t.Errorf("%q.ok() = %v, quero %v", outcome, got, want)
		}
	}
}
