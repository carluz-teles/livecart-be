package integration

import (
	"encoding/json"
	"testing"
)

// UM CAMPO COM O TIPO TROCADO NÃO PODE DERRUBAR O PAYLOAD INTEIRO.
//
// Produção 02/09/2026, cantodaart: o Tiny mandou `numero` como NÚMERO e o
// struct do webhook esperava string. O Unmarshal inteiro caiu, o handler
// seguiu com o struct zerado, e `idPedido` — que estava no corpo — chegou
// vazio ao dispatcher. Nove notas fiscais descartadas num dia com
// "missing idPedido".
//
//	json: cannot unmarshal number into Go struct field .dados.numero of type string
//
// O que este teste guarda não é o campo `numero`: é a regra de que um campo de
// identificação aceita as DUAS grafias, porque o Tiny usa as duas.
func TestNumeroDoPedidoAceitaNumeroEString(t *testing.T) {
	type dados struct {
		IDPedido json.Number   `json:"idPedido"`
		Numero   textoOuNumero `json:"numero"`
	}
	type envelope struct {
		Tipo  string `json:"tipo"`
		Dados dados  `json:"dados"`
	}

	casos := []struct {
		nome   string
		corpo  string
		numero string
	}{
		{"numero como NÚMERO (o caso de produção)",
			`{"tipo":"atualizacao_pedido","dados":{"idPedido":848127017,"numero":27328}}`, "27328"},
		{"numero como STRING",
			`{"tipo":"atualizacao_pedido","dados":{"idPedido":848127017,"numero":"27328"}}`, "27328"},
		{"numero ausente", `{"tipo":"atualizacao_pedido","dados":{"idPedido":848127017}}`, ""},
		{"numero nulo",
			`{"tipo":"atualizacao_pedido","dados":{"idPedido":848127017,"numero":null}}`, ""},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var env envelope
			if err := json.Unmarshal([]byte(c.corpo), &env); err != nil {
				t.Fatalf("payload derrubou o parse: %v", err)
			}
			if got := env.Dados.Numero.String(); got != c.numero {
				t.Errorf("numero = %q, queria %q", got, c.numero)
			}
			// O QUE IMPORTA: o resto do payload sobreviveu. Era isto que se
			// perdia — o idPedido, que é quem resolve o carrinho.
			if env.Dados.IDPedido.String() != "848127017" {
				t.Errorf("idPedido = %q — o campo que decide o carrinho foi junto com o parse",
					env.Dados.IDPedido.String())
			}
		})
	}
}

// Identificador grande não pode passar por float64: 848127017848127017 vira
// outro número. Guarda a decisão de conservar os dígitos como vieram.
func TestNumeroGrandeNaoPerdePrecisao(t *testing.T) {
	var n textoOuNumero
	if err := json.Unmarshal([]byte(`9007199254740993`), &n); err != nil {
		t.Fatal(err)
	}
	if n.String() != "9007199254740993" {
		t.Errorf("numero = %q — passou por float e perdeu dígito", n.String())
	}
}
