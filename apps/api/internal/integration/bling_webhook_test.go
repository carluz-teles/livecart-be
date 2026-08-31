package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// O envelope de referência é o de um evento REAL, capturado da conta do Carlos
// em 29/08/2026. Não é payload inventado: a forma de `data` não está em spec
// nenhum, porque webhook não é endpoint.
const envelopeReal = `{"eventId":"01a05007-571a-7acc-ae53-efcf3cd5ec55",` +
	`"date":"2026-08-30T00:17:33Z","version":"v1","event":"stock.created",` +
	`"companyId":"9db3b9e60022d0eddb121a4319dfbe15",` +
	`"data":{"produto":{"id":16698952209},"saldoFisicoTotal":6,"saldoVirtualTotal":6,` +
	`"deposito":{"id":14889169062,"saldoFisico":6,"saldoVirtual":6},"operacao":"B","quantidade":6}}`

const segredoDoApp = "97e6-secret-do-aplicativo"

func assinarBling(t *testing.T, corpo, segredo string) string {
	t.Helper()
	m := hmac.New(sha256.New, []byte(segredo))
	m.Write([]byte(corpo))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyBlingSignatureDesfechos(t *testing.T) {
	bom := assinarBling(t, envelopeReal, segredoDoApp)

	casos := []struct {
		nome    string
		corpo   string
		header  string
		segredo string
		quer    signatureOutcome
	}{
		{"assinatura correta", envelopeReal, bom, segredoDoApp, signatureValid},
		{"segredo errado", envelopeReal, bom, "outro", signatureMismatch},
		{"corpo adulterado", envelopeReal + " ", bom, segredoDoApp, signatureMismatch},
		{"header ausente", envelopeReal, "", segredoDoApp, signatureMissing},
		{"header só espaço", envelopeReal, "   ", segredoDoApp, signatureMissing},
		{"sem prefixo sha256=", envelopeReal, hex.EncodeToString([]byte("x")), segredoDoApp, signatureMalformed},
		{"hex inválido", envelopeReal, "sha256=zz", segredoDoApp, signatureMalformed},
		{"hex de tamanho errado", envelopeReal, "sha256=abcd", segredoDoApp, signatureMalformed},
		{"sem segredo configurado", envelopeReal, bom, "", signatureUnconfigured},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := verifyBlingSignature([]byte(c.corpo), c.header, c.segredo); got != c.quer {
				t.Fatalf("desfecho = %q, queria %q", got, c.quer)
			}
		})
	}
}

// "Sem segredo" tem de vencer todos os outros desfechos. Se a ordem inverter,
// um deploy sem o secret configurado passaria a reportar "mismatch" e alguém
// concluiria que o Bling manda assinatura errada — quando o defeito é nosso.
func TestSemSegredoVencePrecedenciaNoBling(t *testing.T) {
	for _, h := range []string{"", "lixo", "sha256=zz", assinarBling(t, envelopeReal, segredoDoApp)} {
		if got := verifyBlingSignature([]byte(envelopeReal), h, ""); got != signatureUnconfigured {
			t.Fatalf("com header %q e segredo vazio: %q", h, got)
		}
	}
}

// O envelope real tem de ser parseado inteiro. Se algum campo mudar de nome, o
// roteamento por companyId para de funcionar e a loja deixa de receber evento —
// em silêncio, que é o pior modo de falhar.
func TestEnvelopeRealEhParseadoInteiro(t *testing.T) {
	var env BlingEnvelope
	if err := json.Unmarshal([]byte(envelopeReal), &env); err != nil {
		t.Fatal(err)
	}

	if env.EventID != "01a05007-571a-7acc-ae53-efcf3cd5ec55" {
		t.Errorf("eventId = %q — é ele que casa com UNIQUE(integration_id, event_id)", env.EventID)
	}
	if env.Event != "stock.created" {
		t.Errorf("event = %q", env.Event)
	}
	// MEDIDO: o companyId é byte-idêntico ao data.id de /empresas/me/dados-basicos.
	// É a premissa do roteamento por URL única.
	if env.CompanyID != "9db3b9e60022d0eddb121a4319dfbe15" {
		t.Errorf("companyId = %q — sem ele a URL única não resolve a loja", env.CompanyID)
	}
	if env.Version != "v1" {
		t.Errorf("version = %q, medido %q", env.Version, "v1")
	}

	var d BlingStockData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Produto.ID != 16698952209 {
		t.Errorf("produto = %d", d.Produto.ID)
	}
	// ⭐ O saldo VEM NO PAYLOAD. É o que dispensa um GET de volta e devolve cota
	// para a venda — a diferença que mais alivia uma live contra 3 req/s.
	if d.SaldoFisicoTotal != 6 || d.SaldoVirtualTotal != 6 {
		t.Errorf("o payload devia trazer o saldo: fisico=%v virtual=%v", d.SaldoFisicoTotal, d.SaldoVirtualTotal)
	}
	if d.Deposito.ID != 14889169062 {
		t.Errorf("o payload devia trazer o depósito: %d", d.Deposito.ID)
	}
	if d.Operacao != "B" {
		t.Errorf("operacao = %q", d.Operacao)
	}
}

// O evento de BALANÇO chega como `stock.created`, não `stock.updated` (medido).
// Assinar só `updated` perderia todo ajuste de estoque feito pela tela.
func TestBalancoChegaComoCreatedENaoUpdated(t *testing.T) {
	var env BlingEnvelope
	if err := json.Unmarshal([]byte(envelopeReal), &env); err != nil {
		t.Fatal(err)
	}
	if env.Event == "stock.updated" {
		t.Fatal("o envelope de referência deixou de ser o medido")
	}
	if env.Event != "stock.created" {
		t.Errorf("event = %q; um balanço CRIA registro de estoque, e quem assinar "+
			"só 'updated' perde o ajuste feito pela tela", env.Event)
	}
}

// O roteamento é por RECURSO, e `virtual_stock` tem de cair no mesmo caminho
// de `stock`: a doc diz que o webhook de estoque virtual liga junto com o de
// estoque e herda a configuração — se ele não for tratado, metade dos eventos
// de reserva é descartada em silêncio.
func TestRecursoDoEventoEhOPrefixoAteOPonto(t *testing.T) {
	casos := map[string]string{
		"stock.created":            "stock",
		"stock.updated":            "stock",
		"virtual_stock.updated":    "virtual_stock",
		"product.deleted":          "product",
		"order.created":            "order",
		"consumer_invoice.created": "consumer_invoice",
	}
	for evento, querRecurso := range casos {
		got, _, _ := cortarRecurso(evento)
		if got != querRecurso {
			t.Errorf("recurso de %q = %q, queria %q", evento, got, querRecurso)
		}
	}
}

// ─── FASE 3: o evento de pedido não pode voltar a ser descartado ────────────
//
// O buraco que fechava o ciclo pela metade. O lojista cancelou dois pedidos no
// Bling em 31/08/2026: o estoque de lá voltou certo, e o LiveCart continuou com
// os dois carrinhos vivos segurando 5 unidades — 10 promessas contra 5 peças.
// O evento CHEGAVA e era descartado com um log de "fase 3 pendente".
//
// Catraca de FONTE porque o sintoma é silêncio: o webhook chega, responde 200,
// e nada acontece. Nenhum teste de comportamento falharia.
func TestEventoDePedidoDoBlingNaoEhMaisDescartado(t *testing.T) {
	fonte, err := os.ReadFile("bling_webhook.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fonte), "releitura pendente de implementação") {
		t.Error("o evento de pedido voltou a ser descartado — o cancelamento no ERP " +
			"deixa de chegar ao carrinho, e o carrinho segue segurando peça que o " +
			"ERP já liberou")
	}
	if !strings.Contains(string(fonte), "despacharPedidoBling") {
		t.Error("o handler de pedido sumiu")
	}
}
