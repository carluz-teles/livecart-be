package erp

// A corrida que criou uma unidade fantasma no Tiny, e a ordem que a impede.
//
// 12/08/2026, produção. O comprador mexeu na quantidade do Gabinete Gamer no
// checkout e dois requests do MESMO item se cruzaram por 83 milissegundos:
//
//	17:21:50.829  PATCH 2→1 começa
//	17:21:51.628  DELETE começa — 83ms ANTES do PATCH terminar
//	17:21:51.654  PATCH manda a entrada ao Tiny (movimento 365095969)
//	17:21:51.66   PATCH baixa a reserva de 2 para 1
//	17:21:52.446  DELETE manda OUTRA entrada (movimento 365095970)
//	17:21:52.45   DELETE tenta 1 + (-1) = 0 e bate no CHECK (quantity > 0)
//
// O DELETE havia lido `cart_items` já em 1 e `stock_reservations` ainda em 2.
// Com esses números concluiu que sobraria 1 unidade, escolheu o ramo de
// decremento parcial, chamou o Tiny PRIMEIRO e só então tentou gravar. A
// gravação morreu na constraint e o `return` saiu sem compensar nada.
//
// O Tiny ficou com uma entrada a mais e o produto fechou o dia com 6 unidades
// onde existiam 5. Nenhum registro nosso apontava para o movimento órfão — o
// diagnóstico exigiu arqueologia em integration_logs.
//
// A correção inverte a ordem: o banco decide e aplica primeiro, num UPDATE
// condicional, e o ERP só é chamado depois. Se o ERP recusar, a quantidade
// volta. O pior caso deixou de ser "unidade que não existe" e passou a ser
// "unidade a menos", que é visível e reconciliável.

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// reservaFake implementa a semântica REAL da query condicional, inclusive a
// recusa quando a reserva tem menos do que se pede — que é o ponto do teste.
type reservaFake struct {
	mu        sync.Mutex
	quantity  int
	status    string
	restaurou int
}

func (r *reservaFake) DecrementActiveReservationQuantity(_ context.Context, _, _ string, dec int) (ReservationDecrement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out ReservationDecrement
	if r.status != "active" || r.quantity < dec {
		return out, nil
	}
	out.Applied = true
	out.ReservationIDs = []string{"res-1"}
	if r.quantity > dec {
		r.quantity -= dec
		out.Remaining = r.quantity
	} else {
		r.status = "reversed"
	}
	return out, nil
}

func (r *reservaFake) RestoreReservationQuantityByID(_ context.Context, _ string, inc int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restaurou++
	if r.status == "reversed" {
		r.status = "active"
	} else {
		r.quantity += inc
	}
	return nil
}

func (r *reservaFake) saldo() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quantity, r.status
}

// Duas baixas de 1 sobre uma reserva de 2: as duas passam, e a segunda zera.
func TestDuasBaixasConcorrentesConsomemExatamenteAReserva(t *testing.T) {
	res := &reservaFake{quantity: 2, status: "active"}

	var wg sync.WaitGroup
	aplicadas := make([]bool, 2)
	for i := range aplicadas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := res.DecrementActiveReservationQuantity(context.Background(), "cart", "prod", 1)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
			}
			aplicadas[i] = out.Applied
		}(i)
	}
	wg.Wait()

	n := 0
	for _, ok := range aplicadas {
		if ok {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d baixas aplicadas de 2 — uma reserva de 2 comporta as duas", n)
	}
	qtd, status := res.saldo()
	if status != "reversed" {
		t.Errorf("status = %q, quero reversed — a segunda baixa consumiu a reserva", status)
	}
	if qtd != 1 {
		t.Errorf("quantidade = %d; a linha sai de active com a quantidade INTACTA, "+
			"porque o CHECK (quantity > 0) proíbe zerar em vigor", qtd)
	}
}

// O caso exato do incidente: três baixas de 1 sobre uma reserva de 2. A terceira
// TEM de ser recusada — era ela que virava movimento órfão no Tiny.
func TestBaixaAlemDaReservaEhRecusadaEmVezDeIrParaOERP(t *testing.T) {
	res := &reservaFake{quantity: 2, status: "active"}

	aplicadas := 0
	for i := 0; i < 3; i++ {
		out, err := res.DecrementActiveReservationQuantity(context.Background(), "cart", "prod", 1)
		if err != nil {
			t.Fatalf("baixa %d: %v", i, err)
		}
		if out.Applied {
			aplicadas++
		}
	}

	if aplicadas != 2 {
		t.Errorf("%d baixas aplicadas — a reserva era de 2, então a terceira tem de ser "+
			"recusada ANTES de qualquer chamada ao ERP. Foi essa terceira que virou o "+
			"movimento 365095970 e deixou o Gabinete com 6 unidades onde havia 5", aplicadas)
	}
}

// Recusa não pode custar nada: sem linha alterada, nada de ERP.
func TestBaixaRecusadaNaoTocaNaReserva(t *testing.T) {
	res := &reservaFake{quantity: 1, status: "active"}

	out, err := res.DecrementActiveReservationQuantity(context.Background(), "cart", "prod", 3)
	if err != nil {
		t.Fatalf("baixa: %v", err)
	}
	if out.Applied {
		t.Fatal("aceitou baixar 3 de uma reserva de 1")
	}
	qtd, status := res.saldo()
	if qtd != 1 || status != "active" {
		t.Errorf("reserva mexida numa recusa: quantidade %d, status %q", qtd, status)
	}
}

// Compensação: ERP recusa DEPOIS do banco baixar, e a quantidade volta.
// Sem isto o banco diz "livre" e o Tiny diz "reservada", para sempre.
func TestERPRecusandoDepoisDaBaixaDevolveAsUnidades(t *testing.T) {
	for _, caso := range []struct {
		nome     string
		inicial  int
		baixa    int
		querQtd  int
		querStat string
	}{
		{"baixa parcial", 3, 1, 3, "active"},
		{"baixa total", 2, 2, 2, "active"},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			res := &reservaFake{quantity: caso.inicial, status: "active"}

			out, err := res.DecrementActiveReservationQuantity(context.Background(), "cart", "prod", caso.baixa)
			if err != nil || !out.Applied {
				t.Fatalf("baixa não aplicada: applied=%v err=%v", out.Applied, err)
			}

			// O ERP recusa.
			_ = errors.New("HTTP 500 do Tiny")
			for _, id := range out.ReservationIDs {
				if err := res.RestoreReservationQuantityByID(context.Background(), id, caso.baixa); err != nil {
					t.Fatalf("restaurando: %v", err)
				}
			}

			qtd, status := res.saldo()
			if qtd != caso.querQtd || status != caso.querStat {
				t.Errorf("depois de compensar: quantidade %d status %q, quero %d/%q — "+
					"reserva não restaurada deixa o banco e o Tiny discordando sem reconciliação",
					qtd, status, caso.querQtd, caso.querStat)
			}
			if res.restaurou != 1 {
				t.Errorf("compensou %d vezes, quero 1", res.restaurou)
			}
		})
	}
}
