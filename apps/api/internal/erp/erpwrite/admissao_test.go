package erpwrite

import (
	"fmt"
	"testing"
)

// TestAdmissivelNuncaErraParaMais varre exaustivamente o espaço de saldo × em
// voo. A propriedade é uma só e vale em todo ponto: o admissível nunca pode
// exceder o que o ERP tem menos o que já prometemos.
func TestAdmissivelNuncaErraParaMais(t *testing.T) {
	for saldo := -20; saldo <= 60; saldo++ {
		for voo := 0; voo <= 60; voo++ {
			t.Run(fmt.Sprintf("saldo_%d_voo_%d", saldo, voo), func(t *testing.T) {
				got := Admissivel(saldo, voo)
				if got < 0 {
					t.Fatalf("admissível negativo (%d)", got)
				}
				if got > saldo-voo && !(saldo-voo < 0 && got == 0) {
					t.Fatalf("admissível %d excede saldo-voo (%d)", got, saldo-voo)
				}
				if saldo-voo > 0 && got != saldo-voo {
					t.Fatalf("admissível %d, esperava %d", got, saldo-voo)
				}
			})
		}
	}
}

// TestEspelhoNuncaSobeAcimaDoAdmissivel é a regra que faltava e que produziu o
// oversell de 26/08: o portão partiu de 41 com o ERP em 20.
func TestEspelhoNuncaSobeAcimaDoAdmissivel(t *testing.T) {
	for portao := 0; portao <= 50; portao += 2 {
		for saldo := -10; saldo <= 50; saldo += 3 {
			for voo := 0; voo <= 30; voo += 3 {
				t.Run(fmt.Sprintf("p%d_s%d_v%d", portao, saldo, voo), func(t *testing.T) {
					novo := NovoSaldoDoPortao(portao, saldo, voo)
					teto := Admissivel(saldo, voo)
					if novo > teto {
						t.Fatalf("espelho subiu o portão para %d, acima do admissível %d "+
							"(saldo=%d emVoo=%d) — é assim que se vende a mesma unidade duas vezes",
							novo, teto, saldo, voo)
					}
					if novo < 0 {
						t.Fatalf("portão negativo (%d)", novo)
					}
				})
			}
		}
	}
}

// TestCenarioRealDe2608 reconstrói o caso medido: ERP com 20, canal externo
// levando 8, live com reservas em voo. O portão jamais poderia ter valido 41.
func TestCenarioRealDe2608(t *testing.T) {
	const saldoERPInicial = 20
	casos := []struct {
		nome                    string
		portaoAtual, saldo, voo int
		querMaiorQue            int
	}{
		{"antes da live", 20, saldoERPInicial, 0, -1},
		{"externo levou 8", 20, saldoERPInicial - 8, 0, -1},
		{"externo 8 + 5 reservas em voo", 20, saldoERPInicial - 8, 5, -1},
		{"portao herdado alto de 41", 41, saldoERPInicial, 0, -1},
		{"ERP negativo", 10, -13, 3, -1},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			novo := NovoSaldoDoPortao(c.portaoAtual, c.saldo, c.voo)
			teto := Admissivel(c.saldo, c.voo)
			if novo > teto {
				t.Fatalf("portão %d > admissível %d", novo, teto)
			}
			if c.nome == "portao herdado alto de 41" && novo != 20 {
				t.Fatalf("o portão herdado de 41 tinha de cair para 20 (o saldo do ERP), veio %d", novo)
			}
		})
	}
}

// TestPodeSubirConcordaComNovoSaldo — as duas funções não podem divergir.
func TestPodeSubirConcordaComNovoSaldo(t *testing.T) {
	for portao := 0; portao <= 30; portao++ {
		for saldo := 0; saldo <= 30; saldo += 2 {
			for voo := 0; voo <= 15; voo++ {
				sobe := PodeSubir(portao, saldo, voo)
				novo := NovoSaldoDoPortao(portao, saldo, voo)
				if sobe != (novo > portao) {
					t.Fatalf("PodeSubir=%v mas o portão foi de %d para %d (saldo=%d voo=%d)",
						sobe, portao, novo, saldo, voo)
				}
			}
		}
	}
}
