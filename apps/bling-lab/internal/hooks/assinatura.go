package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Desfecho é o vocabulário de verificação de assinatura, o mesmo que o LiveCart
// já usa para o Instagram (valid|mismatch|missing|malformed|unconfigured).
// Manter o vocabulário permite reaproveitar o modo de observação em dois
// deploys: primeiro só registra, depois passa a recusar.
type Desfecho string

const (
	Valida     Desfecho = "valid"        // bate
	Divergente Desfecho = "mismatch"     // veio, mas não bate — payload adulterado ou secret errado
	Ausente    Desfecho = "missing"      // o header não veio
	Malformada Desfecho = "malformed"    // veio sem o prefixo sha256= ou sem hex válido
	SemSegredo Desfecho = "unconfigured" // não temos client_secret para verificar
)

// HeaderAssinatura é o header que o Bling manda. Case-insensitive no HTTP, mas
// o valor é sempre "sha256=" + hex minúsculo.
const HeaderAssinatura = "X-Bling-Signature-256"

// Verificar confere o HMAC-SHA256 do corpo CRU contra o header.
//
// A chave é o client_secret do aplicativo — o mesmo para TODAS as lojas quando
// o app é único. Consequência que precisa estar escrita em algum lugar: rotacionar
// o secret invalida o Basic do token endpoint E a assinatura de todos os webhooks
// ao mesmo tempo, então a rotação exige janela com dois segredos aceitos.
func Verificar(corpo []byte, header, secret string) Desfecho {
	if secret == "" {
		return SemSegredo
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return Ausente
	}
	valor, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return Malformada
	}
	esperado, err := hex.DecodeString(strings.TrimSpace(valor))
	if err != nil || len(esperado) != sha256.Size {
		return Malformada
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(corpo)
	// hmac.Equal é comparação em tempo constante — a diferença importa mesmo
	// numa ferramenta local, porque este código é o rascunho do de produção.
	if hmac.Equal(mac.Sum(nil), esperado) {
		return Valida
	}
	return Divergente
}

// Assinar produz o header que o Bling produziria. Serve para o replay local:
// reenviar um evento gravado com assinatura VÁLIDA, para exercitar o caminho
// completo sem depender do Bling entregar de novo.
func Assinar(corpo []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(corpo)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
