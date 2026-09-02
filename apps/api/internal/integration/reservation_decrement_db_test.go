package integration

// O decremento condicional da reserva contra o POSTGRES DE VERDADE.
//
// O fake do pacote erp acerta por construção: tem um mutex. Quem precisa
// acertar em produção é o motor, sob transações concorrentes — e é justamente
// aí que o defeito de 12/08/2026 viveu.
//
// O que aconteceu naquele dia, no Gabinete Gamer: um PATCH (2→1) e um DELETE do
// mesmo item de checkout se cruzaram por 83 milissegundos. O código lia a
// quantidade, decidia o ramo (baixa parcial × reversão total), chamava o Tiny e
// só ENTÃO gravava. O DELETE decidiu com a leitura envelhecida — `cart_items`
// já em 1, `stock_reservations` ainda em 2 — mandou a entrada ao Tiny
// (movimento 365095970) e só depois tentou `1 + (-1) = 0`, batendo no
// CHECK (quantity > 0) da migration 000030. O `return` saiu sem compensar.
//
// O produto fechou o dia com 6 unidades no Tiny onde existiam 5, e nada no
// nosso banco apontava para o movimento órfão.
//
// A correção move a decisão para dentro do UPDATE. Este teste prova que o
// UPDATE aguenta a disputa: nunca mais unidades baixadas do que existiam, e
// nunca um 23514.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestDecremento -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
)

func quantidadeDaReserva(t *testing.T, reservationID string) (int, string) {
	t.Helper()
	var qtd int
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT quantity, status FROM stock_reservations WHERE id = $1::uuid`, reservationID,
	).Scan(&qtd, &status); err != nil {
		t.Fatalf("lendo reserva: %v", err)
	}
	return qtd, status
}

// A prova central: N goroutines baixando 1 unidade de uma reserva de K. Só K
// podem passar, as outras têm de ser recusadas LIMPAMENTE — sem exceção, sem
// violar constraint, sem quantidade negativa.
func TestDecrementoConcorrenteNuncaBaixaAlemDaReserva(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	for _, caso := range []struct{ reserva, concorrentes int }{
		{2, 2}, // o caso exato do incidente
		{2, 8},
		{3, 32},
		{1, 16},
	} {
		t.Run(fmt.Sprintf("reserva_%d_com_%d_disputando", caso.reserva, caso.concorrentes), func(t *testing.T) {
			cartID, reservationID := seedReservaAtiva(t, fx, productID, caso.reserva)

			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				aplicadas int
				erros     []error
			)
			largada := make(chan struct{})

			for i := 0; i < caso.concorrentes; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-largada // partem juntas, para a disputa ser real
					out, err := repo.DecrementActiveReservationQuantity(
						context.Background(), cartID, productID, 1)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						erros = append(erros, err)
						return
					}
					if out.Applied {
						aplicadas++
					}
				}()
			}
			close(largada)
			wg.Wait()

			for _, err := range erros {
				// 23514 é a violação do CHECK (quantity > 0) — exatamente o erro
				// que derrubou o DELETE em produção depois do Tiny já ter se movido.
				if strings.Contains(err.Error(), "23514") || strings.Contains(err.Error(), "quantity") {
					t.Errorf("o motor deixou passar uma baixa inválida: %v", err)
				} else {
					t.Errorf("erro inesperado na disputa: %v", err)
				}
			}

			if aplicadas != caso.reserva {
				t.Errorf("%d baixas aplicadas sobre reserva de %d — cada baixa a mais é "+
					"uma entrada a mais no extrato do Tiny, que foi como o Gabinete "+
					"terminou com 6 unidades onde havia 5", aplicadas, caso.reserva)
			}

			qtd, status := quantidadeDaReserva(t, reservationID)
			if status != "reversed" {
				t.Errorf("status = %q, quero reversed — as baixas consumiram a reserva inteira", status)
			}
			if qtd < 1 {
				t.Errorf("quantidade = %d; a linha sai de active com a quantidade INTACTA, "+
					"porque zerar em vigor viola o CHECK (quantity > 0)", qtd)
			}
		})
	}
}

// Baixa parcial mantém a linha viva com o resto certo.
func TestDecrementoParcialMantemAReservaAtiva(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)
	cartID, reservationID := seedReservaAtiva(t, fx, productID, 4)

	out, err := repo.DecrementActiveReservationQuantity(context.Background(), cartID, productID, 2)
	if err != nil {
		t.Fatalf("baixando 2 de 4: %v", err)
	}
	if !out.Applied {
		t.Fatal("recusou baixar 2 de uma reserva de 4")
	}
	if out.Remaining != 2 {
		t.Errorf("Remaining = %d, quero 2", out.Remaining)
	}
	if len(out.ReservationIDs) != 1 || out.ReservationIDs[0] != reservationID {
		t.Errorf("ReservationIDs = %v, quero [%s] — a compensação precisa acertar "+
			"exatamente a linha mexida", out.ReservationIDs, reservationID)
	}

	qtd, status := quantidadeDaReserva(t, reservationID)
	if qtd != 2 || status != "active" {
		t.Errorf("reserva ficou %d/%q, quero 2/active", qtd, status)
	}
}

// Pedir mais do que existe é recusado sem tocar em nada — e sem chamar o ERP,
// que é o ponto: em produção essa chamada saiu ANTES da recusa.
func TestDecrementoAlemDaReservaEhRecusadoSemAlterarNada(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)
	cartID, reservationID := seedReservaAtiva(t, fx, productID, 1)

	out, err := repo.DecrementActiveReservationQuantity(context.Background(), cartID, productID, 3)
	if err != nil {
		t.Fatalf("baixa recusada devolveu erro (deveria ser recusa limpa): %v", err)
	}
	if out.Applied {
		t.Fatal("aceitou baixar 3 de uma reserva de 1")
	}

	qtd, status := quantidadeDaReserva(t, reservationID)
	if qtd != 1 || status != "active" {
		t.Errorf("reserva mexida numa recusa: %d/%q, quero 1/active", qtd, status)
	}
}

// A compensação devolve o que foi baixado, nos dois desfechos.
func TestRestauracaoDevolveAsUnidadesNosDoisDesfechos(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	t.Run("baixa_parcial", func(t *testing.T) {
		cartID, reservationID := seedReservaAtiva(t, fx, productID, 3)
		out, err := repo.DecrementActiveReservationQuantity(context.Background(), cartID, productID, 1)
		if err != nil || !out.Applied {
			t.Fatalf("baixa: applied=%v err=%v", out.Applied, err)
		}
		if err := repo.RestoreReservationQuantityByID(context.Background(), reservationID, 1); err != nil {
			t.Fatalf("restaurando: %v", err)
		}
		if qtd, status := quantidadeDaReserva(t, reservationID); qtd != 3 || status != "active" {
			t.Errorf("depois de compensar: %d/%q, quero 3/active", qtd, status)
		}
	})

	t.Run("baixa_total", func(t *testing.T) {
		cartID, reservationID := seedReservaAtiva(t, fx, productID, 2)
		out, err := repo.DecrementActiveReservationQuantity(context.Background(), cartID, productID, 2)
		if err != nil || !out.Applied {
			t.Fatalf("baixa: applied=%v err=%v", out.Applied, err)
		}
		if err := repo.RestoreReservationQuantityByID(context.Background(), reservationID, 2); err != nil {
			t.Fatalf("restaurando: %v", err)
		}
		// A quantidade ficou intacta na baixa total, então compensar só reabre.
		if qtd, status := quantidadeDaReserva(t, reservationID); qtd != 2 || status != "active" {
			t.Errorf("depois de compensar: %d/%q, quero 2/active — a baixa total não "+
				"mexeu na quantidade, então restaurar não pode somar de novo", qtd, status)
		}
	})
}

// O outro lado da mesma corrida: AUMENTO concorrente.
//
// Em 12/08/2026, no teste do lojista, cliques rápidos no "+" produziram os dois
// desfechos possíveis do par "listar reservas / decidir CREATE ou ADJUST":
//
//	"no rows in result set"                          — leu ativa, sumiu no meio
//	"duplicate key ... uq_stock_reservations_active" — leu vazio, outro criou
//
// Nos dois o movimento já tinha ido ao Tiny e o comprador levava 422 com o
// estoque já mexido. O upsert atômico não escolhe ramo: o índice parcial
// decide, e a soma é uma operação só.
func TestUpsertConcorrenteSomaExatamenteUmaVezPorChamada(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 50, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-ups', 'h-ups', 't-ups-'||floor(random()*1000000)::text,
		         (floor(random()*2000000000))::int, 'active', 'unpaid')
		 RETURNING id::text`, fx.eventID).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	const chamadas = 16
	var wg sync.WaitGroup
	largada := make(chan struct{})
	erros := make(chan error, chamadas)

	for i := 0; i < chamadas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			_, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
				EventID:           fx.eventID,
				CartID:            cartID,
				ProductID:         productID,
				ExternalProductID: "EXT-UPS",
				IncQty:            1,
			})
			if err != nil {
				erros <- err
			}
		}()
	}
	close(largada)
	wg.Wait()
	close(erros)

	for err := range erros {
		t.Errorf("upsert concorrente falhou (era CREATE-vs-ADJUST antes): %v", err)
	}

	var linhas, total int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(quantity),0)::int FROM stock_reservations
		 WHERE cart_id = $1::uuid AND product_id = $2::uuid AND status = 'active'`,
		cartID, productID).Scan(&linhas, &total); err != nil {
		t.Fatalf("lendo reservas: %v", err)
	}
	if linhas != 1 {
		t.Errorf("%d linhas ativas para o mesmo (carrinho, produto), quero 1 — "+
			"linhas empilhadas viram estoque contado duas vezes", linhas)
	}
	if total != chamadas {
		t.Errorf("soma = %d, quero %d — cada chamada soma exatamente uma unidade, "+
			"nem a mais (oversell) nem a menos (unidade perdida)", total, chamadas)
	}
}

// seedReservaAtiva cria um carrinho vencido com uma reserva ATIVA.
//
// Mudou de casa quando a drenagem do modelo legado foi removida: o arquivo que
// a hospedava existia só para provar a reivindicação do estorno contra o
// Postgres, e saiu junto com o código que ele protegia. O helper ficou porque
// os testes de decremento, ciclo de estoque e simulação de live ainda precisam
// semear a linha 'active' que eles próprios consomem.
func seedReservaAtiva(t *testing.T, fx scaleFixture, productID string, qty int) (cartID, reservationID string) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("cl-%d-%d", seedSeq, rand.Intn(1_000_000))

	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, 'h'||$2, 't-'||$2, (floor(random()*2000000000))::int, 'expired', 'unpaid')
		 RETURNING id::text`, fx.eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, status)
		 VALUES ($1, $2, $3, 'EXT-'||$4, $5, 'active')
		 RETURNING id::text`, fx.eventID, cartID, productID, uniq, qty).Scan(&reservationID); err != nil {
		t.Fatalf("seed reserva: %v", err)
	}
	return cartID, reservationID
}
