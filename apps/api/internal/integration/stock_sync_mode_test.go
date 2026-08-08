package integration

// Todas as combinações da regra de sync, mais a viagem de ida e volta completa
// entre os dois jeitos de contar estoque.
//
// O lado que faltava. O estorno já tem simulação em massa (erp), mas ela cobre
// só a IDA — os deltas que mandamos. A VOLTA é o webhook do Tiny com o saldo
// ABSOLUTO, e é nela que os dois números se encontram. Se a volta aplicar uma
// foto tirada no meio de um movimento nosso, o contador local passa a oferecer
// unidade que não existe, e o dano aparece do outro lado: promoção fantasma da
// fila e venda de produto esgotado.

import (
	"fmt"
	"testing"
)

func TestStockSyncModeCobreTodasAsCombinacoes(t *testing.T) {
	// 16 combinações — as quatro entradas booleanas, exaustivas.
	for _, guarded := range []bool{false, true} {
		for _, guardErr := range []bool{false, true} {
			for _, pending := range []bool{false, true} {
				for _, pendErr := range []bool{false, true} {
					nome := fmt.Sprintf("guard=%v/guardErr=%v/pend=%v/pendErr=%v", guarded, guardErr, pending, pendErr)
					t.Run(nome, func(t *testing.T) {
						skip, downgrade := stockSyncMode(guarded, guardErr, pending, pendErr)

						// Invariante 1: skip e downgrade são exclusivos. Os dois
						// juntos seriam uma instrução contraditória para quem
						// aplica ("não mexa" e "mexa só para baixo").
						if skip && downgrade {
							t.Fatalf("%s devolveu skip E downgrade", nome)
						}

						// Invariante 2: erro de consulta NUNCA aplica estoque.
						// Sem saber o que está em voo, qualquer aplicação é
						// aposta, e o lado ruim inventa unidade.
						if (guardErr || pendErr) && !skip {
							t.Errorf("%s: erro de consulta tem de suprimir o sync inteiro", nome)
						}

						// Invariante 3: estorno em voo NUNCA aplica estoque —
						// nem em downgrade. É a regra que a ordem das checagens
						// existe para garantir: downgrade deixaria passar
						// justamente a foto do meio do estorno.
						if pending && !guardErr && !pendErr && !skip {
							t.Errorf("%s: estorno em voo tem de suprimir, não fazer downgrade", nome)
						}

						// Invariante 4: só o caso totalmente limpo aplica o
						// saldo do ERP como veio.
						limpo := !guarded && !guardErr && !pending && !pendErr
						if limpo && (skip || downgrade) {
							t.Errorf("%s: sem nada em voo o sync tem de ser normal", nome)
						}
					})
				}
			}
		}
	}
}

// O caso que motivou o guard: reserva ativa numa live deixa passar redução do
// lojista, mas nunca aumento.
func TestReservaAtivaDeixaPassarSoReducao(t *testing.T) {
	skip, downgrade := stockSyncMode(true, false, false, false)
	if skip {
		t.Error("reserva ativa não pode suprimir o sync — redução do lojista durante a live é legítima")
	}
	if !downgrade {
		t.Error("reserva ativa tem de restringir a downgrade-only, senão o webhook sobe o local e inventa oferta")
	}
}

// -----------------------------------------------------------------------------

// aplicaSaldoDoERP é o modelo do que o sync faz com o número que chegou.
func aplicaSaldoDoERP(local, doERP int, skip, downgradeOnly bool) int {
	if skip {
		return local
	}
	if downgradeOnly && doERP >= local {
		return local
	}
	return doERP
}

// A viagem completa: nossos deltas saem, o Tiny acumula, o webhook volta com o
// saldo, e o local não pode terminar prometendo mais do que existe.
func TestIdaEVoltaEntreDeltaESaldoNuncaInventaUnidade(t *testing.T) {
	casos := []struct {
		nome string
		// estado no momento em que o webhook chega
		localAntes  int
		saldoNoTiny int
		guarded     bool
		pending     bool
		querLocal   int
	}{
		{
			// Nada em voo: o Tiny é a verdade e o local acompanha.
			nome:       "sem movimento nosso, o saldo do ERP manda",
			localAntes: 5, saldoNoTiny: 8,
			querLocal: 8,
		},
		{
			// O caso de staging: cancelamos, creditamos o local de uma vez, e o
			// estorno no ERP ainda está saindo um a um. O Tiny está atrás de
			// nós — aplicar o número dele derrubaria o local.
			nome:       "estorno em voo: saldo menor do Tiny NAO derruba o local",
			localAntes: 5, saldoNoTiny: 4, pending: true,
			querLocal: 5,
		},
		{
			// Mesma janela, direção oposta: o estorno já creditou o Tiny e o
			// número dele passou o nosso. Aplicar criaria a unidade fantasma.
			nome:       "estorno em voo: saldo maior do Tiny NAO sobe o local",
			localAntes: 5, saldoNoTiny: 7, pending: true,
			querLocal: 5,
		},
		{
			// Live rodando com reserva ativa e o lojista tirou unidade do
			// estoque no Tiny. Redução legítima, tem de refletir.
			nome:       "reserva ativa: reducao do lojista reflete",
			localAntes: 5, saldoNoTiny: 3, guarded: true,
			querLocal: 3,
		},
		{
			// Mesma situação, mas o número do Tiny subiu porque a nossa própria
			// reserva foi estornada segundos atrás. Subir aqui promove alguém
			// da fila para uma unidade que não existe.
			nome:       "reserva ativa: aumento NAO sobe o local",
			localAntes: 5, saldoNoTiny: 9, guarded: true,
			querLocal: 5,
		},
		{
			// Empate na janela do guard: nada muda.
			nome:       "reserva ativa: valores iguais nao mexem",
			localAntes: 5, saldoNoTiny: 5, guarded: true,
			querLocal: 5,
		},
		{
			// Estorno vence o guard. Se a ordem das checagens invertesse,
			// downgrade-only deixaria o 4 passar e derrubaria o local.
			nome:       "estorno em voo vence o guard",
			localAntes: 5, saldoNoTiny: 4, guarded: true, pending: true,
			querLocal: 5,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			skip, downgrade := stockSyncMode(c.guarded, false, c.pending, false)
			got := aplicaSaldoDoERP(c.localAntes, c.saldoNoTiny, skip, downgrade)
			if got != c.querLocal {
				t.Errorf("local %d + saldo do ERP %d (guard=%v, estorno=%v) = %d, quero %d",
					c.localAntes, c.saldoNoTiny, c.guarded, c.pending, got, c.querLocal)
			}
		})
	}
}

// Em massa: com QUALQUER movimento nosso em voo, o contador local nunca sobe
// por causa de um webhook. Subir é a única direção que inventa oferta, e é a
// que gera promoção fantasma da fila.
func TestEmMassaWebhookNuncaSobeOLocalComMovimentoEmVoo(t *testing.T) {
	for local := 0; local <= 12; local++ {
		for doERP := 0; doERP <= 12; doERP++ {
			for _, guarded := range []bool{false, true} {
				for _, pending := range []bool{false, true} {
					if !guarded && !pending {
						continue // sem movimento nosso, o ERP manda mesmo
					}
					skip, downgrade := stockSyncMode(guarded, false, pending, false)
					got := aplicaSaldoDoERP(local, doERP, skip, downgrade)
					if got > local {
						t.Fatalf("local %d subiu para %d com saldo %d do ERP (guard=%v, estorno=%v) — unidade inventada",
							local, got, doERP, guarded, pending)
					}
				}
			}
		}
	}
}
