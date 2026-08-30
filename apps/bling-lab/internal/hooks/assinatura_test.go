package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// O payload de referência é o envelope que a doc do Bling declara. O valor
// esperado é calculado aqui e não colado de lugar nenhum: um valor colado só
// prova que alguém sabia digitar, não que a nossa conta bate com a deles.
const payloadRef = `{"eventId":"abc-123","date":"2026-08-29T12:00:00-03:00","version":"1.0",` +
	`"event":"stock.updated","companyId":"436c56a5679921f5f13a3d6433561773","data":{"produto":{"id":123}}}`

const segredoRef = "s3cr3t-do-aplicativo"

func hmacRef(t *testing.T, corpo, segredo string) string {
	t.Helper()
	m := hmac.New(sha256.New, []byte(segredo))
	m.Write([]byte(corpo))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerificarDesfechos(t *testing.T) {
	bom := hmacRef(t, payloadRef, segredoRef)

	casos := []struct {
		nome    string
		corpo   string
		header  string
		segredo string
		quer    Desfecho
	}{
		{"assinatura correta", payloadRef, bom, segredoRef, Valida},
		{"segredo errado", payloadRef, bom, "outro-segredo", Divergente},
		{"corpo adulterado", payloadRef + " ", bom, segredoRef, Divergente},
		{"header ausente", payloadRef, "", segredoRef, Ausente},
		{"header só espaços", payloadRef, "   ", segredoRef, Ausente},
		{"sem prefixo sha256=", payloadRef, hex.EncodeToString([]byte("qualquer")), segredoRef, Malformada},
		{"prefixo certo, hex inválido", payloadRef, "sha256=zzzz", segredoRef, Malformada},
		{"hex válido de tamanho errado", payloadRef, "sha256=abcd", segredoRef, Malformada},
		{"sem segredo configurado", payloadRef, bom, "", SemSegredo},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := Verificar([]byte(c.corpo), c.header, c.segredo); got != c.quer {
				t.Fatalf("Verificar() = %q, queria %q", got, c.quer)
			}
		})
	}
}

// A precedência importa: SEM SEGREDO tem de vencer TODOS os outros desfechos.
// Se um dia alguém inverter a ordem das checagens, um deploy sem o secret
// configurado passaria a reportar "missing"/"mismatch" e alguém concluiria que
// o Bling está mandando assinatura errada — quando o defeito é nosso.
func TestSemSegredoVencePrecedencia(t *testing.T) {
	for _, header := range []string{"", "lixo", "sha256=zzz", hmacRef(t, payloadRef, segredoRef)} {
		if got := Verificar([]byte(payloadRef), header, ""); got != SemSegredo {
			t.Fatalf("com header %q e segredo vazio: %q, queria %q", header, got, SemSegredo)
		}
	}
}

// Assinar tem de produzir exatamente o que Verificar aceita — é o que faz o
// replay local valer como teste do caminho de produção.
func TestAssinarEVerificarFechamOCiclo(t *testing.T) {
	h := Assinar([]byte(payloadRef), segredoRef)
	if got := Verificar([]byte(payloadRef), h, segredoRef); got != Valida {
		t.Fatalf("o header que nós mesmos assinamos não validou: %q", got)
	}
	if len(h) != len("sha256=")+sha256.Size*2 {
		t.Fatalf("formato do header fora do esperado: %q", h)
	}
}

// Corpo vazio é caso de borda real: um POST sem corpo ainda tem HMAC definido,
// e tratá-lo como erro faria o servidor recusar um evento legítimo.
func TestCorpoVazioAindaAssina(t *testing.T) {
	h := Assinar(nil, segredoRef)
	if got := Verificar(nil, h, segredoRef); got != Valida {
		t.Fatalf("corpo vazio deveria validar, veio %q", got)
	}
}
