package erp

// Qual saldo do Tiny é o vendável.
//
// O lojista isolou o caso: o Carrossel Musical Azul estava com estoque físico 4
// e disponível 3 no Tiny, e o LiveCart gravou 4. A unidade que falta está
// reservada por um orçamento salvo — continua no físico e sai do disponível.
// Oferecê-la é vender o que já tem dono.
//
// Nem o webhook nem o GET /produtos resolvem, e isso está medido:
//
//	webhook   {versao, cnpj, tipo, dados{idProduto, sku, nome, saldo}}
//	produtos  estoque{controlar, sobEncomenda, diasPreparacao, localizacao,
//	                  minimo, maximo, quantidade}
//
// Um número só nos dois, idênticos entre si em todos os casos pareados, e os
// dois são o físico. Por isso a consulta separada ao endpoint de estoque.

import "testing"

func TestSaldoFisicoNuncaPassaPorDisponivel(t *testing.T) {
	// É este o campo que causou o furo: mesmo número, nome diferente. Se ele
	// entrasse na lista, pagaríamos uma chamada HTTP a mais para reproduzir
	// exatamente o bug que ela existe para consertar.
	resposta := map[string]any{
		"id":    float64(830590845),
		"saldo": float64(4),
	}
	if _, campo, ok := ExtrairSaldoDisponivel(resposta); ok {
		t.Errorf("aceitou %q como disponível — esse é o saldo FÍSICO, o mesmo 4 que o "+
			"GET /produtos já devolvia quando o Tiny mostrava 3 vendáveis", campo)
	}
}

// A resposta real do Tiny, copiada de produção em 14/08/2026 para o Carrossel
// Musical Azul: o lojista tinha físico 4 com 1 peça reservada num orçamento, e
// só 3 podiam ser vendidas.
func TestRespostaRealDoTiny(t *testing.T) {
	resposta := map[string]any{
		"id":         float64(830590845),
		"nome":       "Carrossel Musical Azul - 17cm",
		"codigo":     "3583A",
		"unidade":    "UN",
		"saldo":      float64(4),
		"reservado":  float64(1),
		"disponivel": float64(3),
		"depositos":  []any{},
	}
	got, campo, ok := ExtrairSaldoDisponivel(resposta)
	if !ok {
		t.Fatal("não resolveu a resposta real do Tiny")
	}
	if campo != "disponivel" {
		t.Errorf("campo = %q, quero \"disponivel\"", campo)
	}
	if got != 3 {
		t.Errorf("saldo = %d, quero 3 — 4 é o físico, e é ele que oferecia a peça "+
			"que já estava reservada num orçamento", got)
	}
}

// Sem campo conhecido, quem chama tem de preservar o saldo físico. Devolver
// zero como se fosse resposta boa esgotaria o catálogo inteiro do lojista.
func TestSemCampoConhecidoNaoAfirmaNada(t *testing.T) {
	for nome, resposta := range map[string]map[string]any{
		"resposta vazia":     {},
		"só campos alheios":  {"id": float64(1), "produto": "x", "depositos": []any{}},
		"disponível textual": {"disponivel": "3"},
		// Só o físico e o reservado, sem o disponível calculado: não é papel
		// desta função inferir a subtração — inferir viraria adivinhação sobre o
		// número que decide venda.
		"sem o disponível": {"saldo": float64(4), "reservado": float64(1)},
	} {
		t.Run(nome, func(t *testing.T) {
			if saldo, _, ok := ExtrairSaldoDisponivel(resposta); ok {
				t.Errorf("afirmou disponível=%d sem campo numérico conhecido — quem chama "+
					"gravaria isso por cima do estoque real", saldo)
			}
		})
	}
}

// Saldo negativo é sintoma, não estoque: o Tiny aceita ir abaixo de zero
// (gravado na bateria de sandbox) e copiar isso propaga o defeito em vez de
// mostrá-lo. Cair para o físico deixa o número errado visível no lugar certo.
func TestNegativoNaoEntra(t *testing.T) {
	if saldo, _, ok := ExtrairSaldoDisponivel(map[string]any{"disponivel": float64(-2)}); ok {
		t.Errorf("aceitou disponível=%d — negativo é saída além do que existia, "+
			"não um número a espelhar", saldo)
	}
}

// Zero é saldo legítimo e precisa passar: é exatamente o produto esgotado, o
// caso em que oferecer uma unidade a mais dói mais.
func TestZeroEhSaldoValido(t *testing.T) {
	saldo, _, ok := ExtrairSaldoDisponivel(map[string]any{"disponivel": float64(0)})
	if !ok {
		t.Fatal("recusou zero — o produto esgotado voltaria a ser oferecido pelo físico")
	}
	if saldo != 0 {
		t.Errorf("saldo = %d, quero 0", saldo)
	}
}
