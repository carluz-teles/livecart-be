package integration

// A reivindicação do estorno contra o POSTGRES DE VERDADE.
//
// As simulações do pacote erp provam a lógica contra um fake com mutex. Isso
// deixa de fora justamente a parte que carrega o peso: o `UPDATE ... WHERE
// status = 'active'` ser atômico no motor, sob transações concorrentes. Um
// mutex em Go acerta por construção; o banco é quem tem de acertar em produção.
//
// É a diferença entre "a nossa lógica está certa" e "o mecanismo funciona".
// Em 08/08 a lógica também parecia certa.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestReivindicacao -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
)

// seedReservaAtiva cria cart + reserva ativa prontos para a disputa.
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

func statusDaReserva(t *testing.T, reservationID string) string {
	t.Helper()
	var st string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM stock_reservations WHERE id = $1::uuid`, reservationID).Scan(&st); err != nil {
		t.Fatalf("lendo status: %v", err)
	}
	return st
}

// TestReivindicacaoSoUmVencedorNoPostgres é a prova do mecanismo: sob N
// goroutines disputando a MESMA reserva, o banco só pode deixar uma passar.
//
// Cada vencedor a mais aqui seria uma entrada a mais no extrato do Tiny — foi
// exatamente assim que a reserva f4590b1f virou duas linhas de 2 unidades e um
// produto de 5 terminou com 7.
func TestReivindicacaoSoUmVencedorNoPostgres(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)

	for _, concorrentes := range []int{2, 8, 32} {
		t.Run(fmt.Sprintf("%d_disputando", concorrentes), func(t *testing.T) {
			_, reservationID := seedReservaAtiva(t, fx, productID, 2)
			repo := NewRepository(sqlc.New(testPool), testPool)

			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				vencedores int
				erros     []error
			)
			largada := make(chan struct{})

			for i := 0; i < concorrentes; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-largada // todos partem juntos, para a disputa ser real
					ok, err := repo.ClaimReservationForReversal(context.Background(), reservationID)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						erros = append(erros, err)
						return
					}
					if ok {
						vencedores++
					}
				}()
			}
			close(largada)
			wg.Wait()

			for _, err := range erros {
				t.Errorf("erro na reivindicação: %v", err)
			}
			if vencedores != 1 {
				t.Errorf("%d goroutines disputando devolveram %d vencedores, quero exatamente 1 — "+
					"cada excedente vira uma entrada duplicada no ERP", concorrentes, vencedores)
			}
			if st := statusDaReserva(t, reservationID); st != "reversed" {
				t.Errorf("status da reserva = %q, quero 'reversed'", st)
			}
		})
	}
}

// Reivindicar de novo o que já foi reivindicado devolve false — é o caso da
// retentativa da asynq, e é o que a impede de reenviar a entrada.
func TestReivindicacaoRepetidaNaoVenceDeNovo(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	_, reservationID := seedReservaAtiva(t, fx, productID, 3)
	repo := NewRepository(sqlc.New(testPool), testPool)
	ctx := context.Background()

	primeira, err := repo.ClaimReservationForReversal(ctx, reservationID)
	if err != nil || !primeira {
		t.Fatalf("primeira reivindicação = %v, err=%v; queria true", primeira, err)
	}
	// A asynq retenta até três vezes.
	for i := 0; i < 3; i++ {
		again, err := repo.ClaimReservationForReversal(ctx, reservationID)
		if err != nil {
			t.Fatalf("retentativa %d: %v", i+1, err)
		}
		if again {
			t.Fatalf("retentativa %d venceu de novo — o ERP receberia a mesma entrada outra vez", i+1)
		}
	}
}

// O ERP recusou: a restauração devolve a reserva ao radar, e uma nova
// reivindicação vence. Sem isso a unidade ficaria presa fora do ERP para
// sempre, porque nenhuma tentativa futura voltaria a enxergá-la.
func TestRestauracaoDevolveAReservaAoRadar(t *testing.T) {
	requireDB(t)

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	_, reservationID := seedReservaAtiva(t, fx, productID, 1)
	repo := NewRepository(sqlc.New(testPool), testPool)
	ctx := context.Background()

	if ok, err := repo.ClaimReservationForReversal(ctx, reservationID); err != nil || !ok {
		t.Fatalf("reivindicação inicial falhou: ok=%v err=%v", ok, err)
	}
	if err := repo.RestoreReservationToActive(ctx, reservationID); err != nil {
		t.Fatalf("restauração: %v", err)
	}
	if st := statusDaReserva(t, reservationID); st != "active" {
		t.Fatalf("status após restaurar = %q, quero 'active'", st)
	}
	ok, err := repo.ClaimReservationForReversal(ctx, reservationID)
	if err != nil || !ok {
		t.Errorf("nova reivindicação após restaurar = %v (err=%v), quero true — a retentativa precisa conseguir concluir", ok, err)
	}
}
