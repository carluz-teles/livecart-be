package integration

// A JANELA do guard de estoque, contra o Postgres real.
//
// O bug de campo, 08/08, produto "Café em Grao Torrado". Três reservas saíram e
// voltaram — 10 unidades para fora, 10 de volta, conta fechada — e mesmo assim
// o contador local terminou em 6 enquanto todos os outros produtos ficaram em 5.
//
// O extra veio do sync. Às 18:38:15.693 o webhook da Tiny escreveu o saldo
// ABSOLUTO por cima do local (0 → 1), um milissegundo antes de sairmos com mais
// uma reserva. O guard, que existe exatamente para impedir isso, reportou
// `downgrade_only=false`.
//
// Ele não viu a reserva porque exigia `le.status = 'active'` além de
// `sr.status = 'active'`. Só que a reserva SOBREVIVE ao fim do evento: quando a
// campanha fecha, o carrinho vai para 'checkout' e continua segurando a unidade
// no Tiny até expirar ou ser pago. No caso do Café isso durou 22 minutos — das
// 18:38:11 às 19:00:32 — com o evento já 'ended' e o guard desligado.
//
// Enquanto a reserva está ativa nós seguramos a peça no ERP, e o saldo dele
// está atrás do nosso por nossa causa. O status do evento não muda esse fato.
//
// Rodar:
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestGuard -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
)

// seedGuardCenario cria produto + cart + reserva ativa num evento com o status
// pedido, e devolve o external id do produto.
func seedGuardCenario(t *testing.T, statusEvento string) (storeID, externalID, reservationID string) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("g-%d-%d", seedSeq, rand.Intn(1_000_000))

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Guard', 'guard-'||$1) RETURNING id::text`, uniq).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1, $2, 'Guard', now() + interval '1 day') RETURNING id::text`,
		storeID, statusEvento).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	externalID = "EXT-" + uniq
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Cafe', 'tiny', $2, $3, 1000, 5) RETURNING id::text`,
		storeID, externalID, fmt.Sprintf("G%03d", seedSeq%1000)).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid')
		 RETURNING id::text`, eventID, uniq).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, status)
		 VALUES ($1, $2, $3, $4, 1, 'active') RETURNING id::text`,
		eventID, cartID, productID, externalID).Scan(&reservationID); err != nil {
		t.Fatalf("seed reserva: %v", err)
	}
	return storeID, externalID, reservationID
}

// TestGuardProtegeComEventoEncerrado é o caso de campo, exatamente.
func TestGuardProtegeComEventoEncerrado(t *testing.T) {
	requireDB(t)
	repo := NewRepository(sqlc.New(testPool), testPool)
	ctx := context.Background()

	for _, statusEvento := range []string{"active", "ended", "scheduled"} {
		t.Run("evento_"+statusEvento, func(t *testing.T) {
			storeID, externalID, _ := seedGuardCenario(t, statusEvento)

			guarded, err := repo.HasStockGuardForProduct(ctx, externalID, storeID, "tiny")
			if err != nil {
				t.Fatalf("guard: %v", err)
			}
			if !guarded {
				t.Errorf("evento %q com reserva ATIVA não acionou o guard — "+
					"o saldo absoluto do ERP sobrescreveria o contador local enquanto ainda seguramos a peça",
					statusEvento)
			}
		})
	}
}

// Sem reserva ativa não há o que proteger: o ERP volta a ser fonte da verdade.
func TestGuardLiberaQuandoNadaEstaSeguro(t *testing.T) {
	requireDB(t)
	repo := NewRepository(sqlc.New(testPool), testPool)
	ctx := context.Background()

	storeID, externalID, reservationID := seedGuardCenario(t, "ended")
	if _, err := testPool.Exec(ctx,
		`UPDATE stock_reservations SET status='reversed', reversed_at=now() WHERE id=$1::uuid`, reservationID); err != nil {
		t.Fatalf("estornando: %v", err)
	}

	guarded, err := repo.HasStockGuardForProduct(ctx, externalID, storeID, "tiny")
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if guarded {
		t.Error("sem reserva ativa o guard não pode segurar o sync — redução legítima do lojista pararia de refletir")
	}
}

// A CORRIDA de campo: o webhook chega no mesmo instante em que a reserva é
// criada. Enquanto existir reserva ativa, toda consulta ao guard tem de
// responder TRUE — se uma só responder FALSE, aquele webhook sobrescreve o
// contador e inventa a unidade.
func TestGuardNaoAbreJanelaSobConcorrencia(t *testing.T) {
	requireDB(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	storeID, externalID, _ := seedGuardCenario(t, "ended")

	const leitores = 40
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		abriu    int
		erros    []error
	)
	largada := make(chan struct{})
	for i := 0; i < leitores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			guarded, err := repo.HasStockGuardForProduct(context.Background(), externalID, storeID, "tiny")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				erros = append(erros, err)
				return
			}
			if !guarded {
				abriu++
			}
		}()
	}
	close(largada)
	wg.Wait()

	for _, err := range erros {
		t.Errorf("erro no guard: %v", err)
	}
	if abriu != 0 {
		t.Errorf("%d de %d consultas concorrentes abriram a janela — cada uma é um webhook que sobrescreve o local",
			abriu, leitores)
	}
}

// Duas lojas com o MESMO id externo no ERP não podem se contaminar: o guard de
// uma não vale para a outra.
func TestGuardNaoVazaEntreLojas(t *testing.T) {
	requireDB(t)
	repo := NewRepository(sqlc.New(testPool), testPool)
	ctx := context.Background()

	storeA, externalID, _ := seedGuardCenario(t, "ended")

	// Loja B, mesmo id externo, sem reserva nenhuma.
	seedSeq++
	uniq := fmt.Sprintf("gb-%d-%d", seedSeq, rand.Intn(1_000_000))
	var storeB string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('GuardB', 'guardb-'||$1) RETURNING id::text`, uniq).Scan(&storeB); err != nil {
		t.Fatalf("seed store B: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Cafe B', 'tiny', $2, $3, 1000, 5)`,
		storeB, externalID, fmt.Sprintf("H%03d", seedSeq%1000)); err != nil {
		t.Fatalf("seed product B: %v", err)
	}

	if guarded, err := repo.HasStockGuardForProduct(ctx, externalID, storeA, "tiny"); err != nil || !guarded {
		t.Errorf("loja A devia estar protegida: guarded=%v err=%v", guarded, err)
	}
	if guarded, err := repo.HasStockGuardForProduct(ctx, externalID, storeB, "tiny"); err != nil || guarded {
		t.Errorf("loja B não tem reserva e não pode herdar o guard da A: guarded=%v err=%v", guarded, err)
	}
}
