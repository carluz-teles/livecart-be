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

func TestReconheceOsNomesCandidatos(t *testing.T) {
	for _, campo := range []string{
		"saldoDisponivel", "saldo_disponivel", "disponivel", "quantidadeDisponivel",
	} {
		t.Run(campo, func(t *testing.T) {
			got, usado, ok := ExtrairSaldoDisponivel(map[string]any{
				"saldo": float64(4),
				campo:   float64(3),
			})
			if !ok {
				t.Fatalf("não reconheceu %q", campo)
			}
			if usado != campo {
				t.Errorf("usou %q em vez de %q", usado, campo)
			}
			if got != 3 {
				t.Errorf("saldo = %d, quero 3 (o disponível, não o físico 4)", got)
			}
		})
	}
}

// Sem campo conhecido, quem chama tem de preservar o saldo físico. Devolver
// zero como se fosse resposta boa esgotaria o catálogo inteiro do lojista.
func TestSemCampoConhecidoNaoAfirmaNada(t *testing.T) {
	for nome, resposta := range map[string]map[string]any{
		"resposta vazia":     {},
		"só campos alheios":  {"id": float64(1), "produto": "x", "depositos": []any{}},
		"disponível textual": {"disponivel": "3"},
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
	if saldo, _, ok := ExtrairSaldoDisponivel(map[string]any{"saldoDisponivel": float64(-2)}); ok {
		t.Errorf("aceitou disponível=%d — negativo é saída além do que existia, "+
			"não um número a espelhar", saldo)
	}
}

// Zero é saldo legítimo e precisa passar: é exatamente o produto esgotado, o
// caso em que oferecer uma unidade a mais dói mais.
func TestZeroEhSaldoValido(t *testing.T) {
	saldo, _, ok := ExtrairSaldoDisponivel(map[string]any{"saldoDisponivel": float64(0)})
	if !ok {
		t.Fatal("recusou zero — o produto esgotado voltaria a ser oferecido pelo físico")
	}
	if saldo != 0 {
		t.Errorf("saldo = %d, quero 0", saldo)
	}
}

// A ordem importa quando o Tiny manda mais de um nome: o mais específico ganha.
func TestPrefereONomeMaisEspecifico(t *testing.T) {
	_, campo, ok := ExtrairSaldoDisponivel(map[string]any{
		"disponivel":      float64(9),
		"saldoDisponivel": float64(3),
	})
	if !ok {
		t.Fatal("não resolveu")
	}
	if campo != "saldoDisponivel" {
		t.Errorf("escolheu %q; com os dois presentes o mais específico é que vale", campo)
	}
}
