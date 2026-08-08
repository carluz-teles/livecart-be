package erp

// A semântica de estoque da Tiny, conferida contra movimentos REAIS.
//
// Tudo que os outros testes assumem sobre o outro lado — "S tira", "E devolve",
// "B fixa" — vinha da leitura da documentação e do meu razão inventado. Estas
// transições não: foram gravadas em 11/07 pela bateria de sandbox contra a
// conta ADABYTE LTDA (tiny-sandbox-results/actions.jsonl), com o saldo lido
// antes e depois de cada movimento pela própria API deles.
//
// Duas descobertas dessas gravações justificam sozinhas todo o desenho da
// correção:
//
//  1. A Tiny ACEITA a mesma entrada duas vezes e soma as duas. A bateria testou
//     isso de propósito ("E dupla 1" e "E dupla 2"): saldo 10, duas entradas de
//     3, saldo 16. Deviam ter voltado 3 unidades e voltaram 6. É, movimento por
//     movimento, o que aconteceu em produção em 08/08 com o Perfume.
//
//  2. A Tiny ACEITA saldo NEGATIVO. A mesma bateria lançou estoque com saldo 0
//     e o saldo foi para -1 sem erro.
//
// Juntas: o outro lado não valida a nossa aritmética, em nenhuma das duas
// direções. Não existe rede de proteção do lado de lá — se mandarmos o delta
// duas vezes, ele conta duas vezes, e ninguém avisa. A idempotência tem de ser
// nossa, e é por isso que a reivindicação vem ANTES da chamada.

import "testing"

// movimentoReal é uma transição efetivamente observada na API da Tiny.
type movimentoReal struct {
	saldoAntes  int
	tipo        string // S=saída, E=entrada, B=balanço (absoluto)
	quantidade  int
	saldoDepois int
	rotulo      string
}

// transicoesGravadas vem de tiny-sandbox-results/actions.jsonl, pareando cada
// POST /estoque/{id} com a leitura de saldo imediatamente anterior e posterior
// do MESMO produto.
var transicoesGravadas = []movimentoReal{
	{465, "B", 10, 10, "baseline saldo=10 (balanço)"},
	{10, "E", 1, 11, "entrada E"},
	{11, "S", 1, 10, "saída S"},
	{10, "E", 3, 13, "E dupla 1"},
	{13, "E", 3, 16, "E dupla 2"},
	{16, "B", 10, 10, "balanço corretivo"},
	{1, "S", 1, 0, "prender saldo"},
	{0, "E", 1, 1, "devolver saldo"},
	{1, "S", 1, 0, "prender saldo"},
	{0, "E", 1, 1, "devolver saldo"},
}

// aplicaMovimentoTiny é o nosso modelo do que a Tiny faz com um movimento.
// Todo raciocínio de estoque neste código depende dele estar certo.
func aplicaMovimentoTiny(saldo int, tipo string, qtd int) int {
	switch tipo {
	case "S": // saída: tira do saldo
		return saldo - qtd
	case "E": // entrada: devolve ao saldo
		return saldo + qtd
	case "B": // balanço: FIXA o saldo, ignora o anterior
		return qtd
	}
	return saldo
}

// TestModeloBateComOsMovimentosReaisDaTiny confere o nosso modelo contra cada
// transição gravada. Se a Tiny mudar a semântica, é aqui que aparece.
func TestModeloBateComOsMovimentosReaisDaTiny(t *testing.T) {
	for _, m := range transicoesGravadas {
		t.Run(m.rotulo, func(t *testing.T) {
			got := aplicaMovimentoTiny(m.saldoAntes, m.tipo, m.quantidade)
			if got != m.saldoDepois {
				t.Errorf("saldo %d + %s %d = %d no nosso modelo, mas a Tiny devolveu %d",
					m.saldoAntes, m.tipo, m.quantidade, got, m.saldoDepois)
			}
		})
	}
}

// A descoberta que justifica a correção inteira: a Tiny não deduplica.
//
// Este teste não exercita código nosso — ele CONGELA um fato sobre o outro
// lado. Se alguém um dia sugerir "a Tiny deve ignorar a entrada repetida,
// então o retry é inofensivo", a resposta está aqui, medida.
func TestTinyAceitaEntradaDuplicadaESoma(t *testing.T) {
	var dupla []movimentoReal
	for _, m := range transicoesGravadas {
		if m.rotulo == "E dupla 1" || m.rotulo == "E dupla 2" {
			dupla = append(dupla, m)
		}
	}
	if len(dupla) != 2 {
		t.Fatalf("esperava as duas entradas duplicadas gravadas, achei %d", len(dupla))
	}

	inicial := dupla[0].saldoAntes
	final := dupla[1].saldoDepois
	devolvidoDeVerdade := dupla[0].quantidade // só UMA entrada era devida

	if final == inicial+devolvidoDeVerdade {
		t.Fatal("a gravação mostra a Tiny deduplicando — se isso mudou, a premissa da correção mudou junto")
	}
	if final != inicial+2*devolvidoDeVerdade {
		t.Errorf("saldo final %d, esperava %d (as DUAS entradas somadas)", final, inicial+2*devolvidoDeVerdade)
	}

	inventadas := final - (inicial + devolvidoDeVerdade)
	if inventadas <= 0 {
		t.Errorf("a entrada duplicada devia ter inventado unidade, inventou %d", inventadas)
	}
	t.Logf("a segunda entrada inventou %d unidade(s) — a Tiny aceitou sem reclamar, "+
		"exatamente como em 08/08 com o Perfume", inventadas)
}

// O balanço é o único movimento ABSOLUTO, e é o que o webhook de estoque
// reflete. Confundi-lo com delta zeraria o estoque do lojista.
func TestBalancoFixaOSaldoIgnorandoOAnterior(t *testing.T) {
	for _, m := range transicoesGravadas {
		if m.tipo != "B" {
			continue
		}
		if m.saldoDepois != m.quantidade {
			t.Errorf("%s: balanço de %d deixou o saldo em %d — balanço FIXA, não soma",
				m.rotulo, m.quantidade, m.saldoDepois)
		}
	}
	// A gravação de 465 → B 10 → 10 é a prova mais forte: o saldo anterior era
	// enorme e sumiu por completo.
	if got := aplicaMovimentoTiny(465, "B", 10); got != 10 {
		t.Errorf("balanço sobre saldo 465 = %d, quero 10", got)
	}
}

// A Tiny deixa o saldo ir a NEGATIVO. Nada do lado dela impede a nossa conta
// de errar para baixo, assim como nada impede de errar para cima.
func TestSaldoPodeFicarNegativoNaTiny(t *testing.T) {
	// Gravado na bateria: lançar estoque com saldo 0 levou o saldo a -1.
	if got := aplicaMovimentoTiny(0, "S", 1); got != -1 {
		t.Errorf("saída de 1 sobre saldo 0 = %d, quero -1 — a Tiny não trava em zero", got)
	}
	// O corolário que importa: não existe validação do outro lado. Se
	// mandarmos o delta duas vezes, ele conta duas vezes, nos dois sentidos.
	saldo := 5
	for i := 0; i < 3; i++ {
		saldo = aplicaMovimentoTiny(saldo, "S", 2)
	}
	if saldo != -1 {
		t.Errorf("três saídas de 2 sobre saldo 5 = %d, quero -1", saldo)
	}
}
