package user

import (
	"fmt"
	"testing"
	"time"
)

// Vetor oficial do Svix (o mesmo usado nas libs deles). Serve de âncora: se
// alguém reescrever o cálculo e errar o formato do conteúdo assinado, a chave
// ou a codificação da saída, este teste quebra — e não a produção.
const (
	vectorSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vectorMsgID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTimestamp = "1614265330"
	vectorPayload   = `{"test": 2432232314}`
	vectorSignature = "g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestSvixSignatureMatchesOfficialVector(t *testing.T) {
	got, err := svixSignature(vectorSecret, vectorMsgID, vectorTimestamp, []byte(vectorPayload))
	if err != nil {
		t.Fatalf("svixSignature returned error: %v", err)
	}

	if got != vectorSignature {
		t.Errorf("signature mismatch\n got: %s\nwant: %s", got, vectorSignature)
	}
}

func TestSvixSignatureRejectsMalformedSecret(t *testing.T) {
	if _, err := svixSignature("whsec_not-valid-base64!!", vectorMsgID, vectorTimestamp, []byte(vectorPayload)); err == nil {
		t.Error("expected an error for a secret that is not base64")
	}
}

// signedNow produz um header de assinatura válido para "agora", já que o vetor
// oficial é de 2021 e cai fora da janela de tolerância.
func signedNow(t *testing.T, payload string) (msgID, timestamp, header string) {
	t.Helper()

	msgID = "msg_test_00000000000000000000"
	timestamp = fmt.Sprintf("%d", time.Now().Unix())

	sig, err := svixSignature(vectorSecret, msgID, timestamp, []byte(payload))
	if err != nil {
		t.Fatalf("building signature: %v", err)
	}

	return msgID, timestamp, "v1," + sig
}

func TestVerifyWebhookSignatureAcceptsFreshDelivery(t *testing.T) {
	payload := `{"type":"user.created"}`
	msgID, timestamp, header := signedNow(t, payload)

	if !verifyWebhookSignature([]byte(payload), msgID, timestamp, header, vectorSecret) {
		t.Error("expected a freshly signed delivery to be accepted")
	}
}

func TestVerifyWebhookSignatureAcceptsOneOfSeveralSignatures(t *testing.T) {
	payload := `{"type":"user.created"}`
	msgID, timestamp, header := signedNow(t, payload)

	// Durante rotação de segredo o Svix manda as duas assinaturas no header.
	rotating := "v1,c3RhbGUtc2lnbmF0dXJl " + header

	if !verifyWebhookSignature([]byte(payload), msgID, timestamp, rotating, vectorSecret) {
		t.Error("expected a header with several signatures to be accepted when one matches")
	}
}

func TestVerifyWebhookSignatureRejects(t *testing.T) {
	payload := `{"type":"user.created"}`
	msgID, timestamp, header := signedNow(t, payload)

	stale := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	staleSig, err := svixSignature(vectorSecret, msgID, stale, []byte(payload))
	if err != nil {
		t.Fatalf("building stale signature: %v", err)
	}

	cases := []struct {
		name      string
		payload   string
		msgID     string
		timestamp string
		header    string
		secret    string
	}{
		{"corpo adulterado", `{"type":"user.deleted"}`, msgID, timestamp, header, vectorSecret},
		{"segredo errado", payload, msgID, timestamp, header, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSx"},
		{"msg id trocado", payload, "msg_outro", timestamp, header, vectorSecret},
		{"timestamp fora da janela", payload, msgID, stale, "v1," + staleSig, vectorSecret},
		{"header ausente", payload, msgID, timestamp, "", vectorSecret},
		{"svix-id ausente", payload, "", timestamp, header, vectorSecret},
		{"timestamp ausente", payload, msgID, "", header, vectorSecret},
		{"timestamp nao numerico", payload, msgID, "ontem", header, vectorSecret},
		{"versao desconhecida", payload, msgID, timestamp, "v2," + header[len("v1,"):], vectorSecret},
		{"header sem virgula", payload, msgID, timestamp, header[len("v1,"):], vectorSecret},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if verifyWebhookSignature([]byte(tc.payload), tc.msgID, tc.timestamp, tc.header, tc.secret) {
				t.Error("expected the delivery to be rejected")
			}
		})
	}
}
