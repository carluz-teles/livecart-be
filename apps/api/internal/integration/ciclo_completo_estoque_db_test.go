package integration

// O ciclo que o lojista vive, contra o Postgres de verdade, com o invariante
// que ele cobrou: cancelar tudo tem de devolver o saldo ao que era.
//
// Reproduz o incidente de 12/08/2026. Naquele dia todos os produtos começaram
// com 5 unidades no Tiny, três carrinhos foram criados numa live, o comprador
// mexeu nas quantidades no checkout, e os três carrinhos foram cancelados. O
// saldo deveria ter voltado a 5 em tudo. Fechou com o Gabinete Gamer em 6 e o
// Perfume Cebolinha em 4 — uma unidade inventada e uma unidade sumida, que só
// não apareceram no agregado porque tinham sinais opostos.
//
// Este teste exercita as MESMAS operações contra o banco real: reserva por
// comentário, aumento por comentário repetido, redução no checkout (a operação
// que o lojista aponta como "sempre dá problema"), remoção de item, e o estorno
// do cancelamento. O razão do ERP é simulado por um livro que aplica S e E como
// o Tiny aplica — inclusive aceitando entrada duplicada e saldo negativo sem
// reclamar, que é o que o Tiny faz de verdade e o que torna o erro silencioso.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestCicloCompleto -v ./apps/api/internal/integration/

import (
	"context"
	"sync"

	"livecart/apps/api/db/sqlc"
	"testing"
)

// razaoDoTiny é o extrato do ERP: aplica os deltas que mandamos, sem julgar.
// O Tiny aceita entrada duplicada e saldo negativo — foi essa complacência que
// deixou o erro de 12/08 passar despercebido por horas.
type razaoDoTiny struct {
	mu     sync.Mutex
	saldo  map[string]int
	movtos []string
}

func novoRazao(produtos map[string]int) *razaoDoTiny {
	r := &razaoDoTiny{saldo: map[string]int{}}
	for id, q := range produtos {
		r.saldo[id] = q
	}
	return r
}

func (r *razaoDoTiny) saida(produto string, qtd int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saldo[produto] -= qtd
	r.movtos = append(r.movtos, "S")
}

func (r *razaoDoTiny) entrada(produto string, qtd int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saldo[produto] += qtd
	r.movtos = append(r.movtos, "E")
}

func (r *razaoDoTiny) ler(produto string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saldo[produto]
}

// O invariante central. Cada passo mexe na reserva pelo caminho REAL do
// repositório e espelha o movimento correspondente no razão, exatamente como o
// serviço faz: o banco decide primeiro, o ERP recebe depois.
func TestCicloCompletoDeVendaVoltaAoSaldoInicial(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const saldoInicial = 5
	razao := novoRazao(map[string]int{productID: saldoInicial})

	// --- Carrinho 1: comentário reserva 1, comentário repetido sobe para 3 ---
	cart1, _ := seedReservaAtiva(t, fx, productID, 1)
	razao.saida(productID, 1)

	if _, err := repo.AdjustActiveReservationQuantity(ctx, cart1, productID, 2, "mov-bump"); err != nil {
		t.Fatalf("aumento por comentário repetido: %v", err)
	}
	razao.saida(productID, 2)

	// --- Carrinho 2: reserva 2 ---
	cart2, _ := seedReservaAtiva(t, fx, productID, 2)
	razao.saida(productID, 2)

	if got := razao.ler(productID); got != 0 {
		t.Fatalf("saldo no Tiny = %d depois de reservar 5 de 5, quero 0", got)
	}

	// --- Checkout do carrinho 1: reduz de 3 para 1 (a operação que quebrava) ---
	dec1, err := repo.DecrementActiveReservationQuantity(ctx, cart1, productID, 2)
	if err != nil {
		t.Fatalf("redução no checkout: %v", err)
	}
	if !dec1.Applied {
		t.Fatal("redução de 2 sobre reserva de 3 foi recusada")
	}
	if dec1.Remaining != 1 {
		t.Errorf("sobrou %d, quero 1", dec1.Remaining)
	}
	razao.entrada(productID, 2)

	// --- Checkout do carrinho 2: remove o item inteiro ---
	dec2, err := repo.DecrementActiveReservationQuantity(ctx, cart2, productID, 2)
	if err != nil {
		t.Fatalf("remoção de item: %v", err)
	}
	if !dec2.Applied {
		t.Fatal("remoção do item foi recusada")
	}
	if dec2.Remaining != 0 {
		t.Errorf("sobrou %d depois de remover o item, quero 0", dec2.Remaining)
	}
	razao.entrada(productID, 2)

	// --- Cancelamento: estorna o que cada carrinho ainda segura ---
	// Só o carrinho 1 tem reserva viva (1 unidade); o 2 já foi zerado acima e
	// não pode ser estornado de novo. Estornar duas vezes é o defeito que a
	// reivindicação claim-first existe para impedir.
	sobra1 := reservasAtivasDoCarrinho(t, cart1, productID)
	if sobra1 != 1 {
		t.Fatalf("carrinho 1 segura %d, quero 1", sobra1)
	}
	if err := repo.ReverseReservationsByCartAndProduct(ctx, cart1, productID); err != nil {
		t.Fatalf("estorno do cancelamento: %v", err)
	}
	razao.entrada(productID, sobra1)

	sobra2 := reservasAtivasDoCarrinho(t, cart2, productID)
	if sobra2 != 0 {
		t.Fatalf("carrinho 2 ainda segura %d depois da remoção — estornar de novo "+
			"criaria unidade do nada", sobra2)
	}

	// --- O invariante ---
	if got := razao.ler(productID); got != saldoInicial {
		t.Errorf("SALDO FINAL NO TINY = %d, quero %d.\n"+
			"Todos os carrinhos foram cancelados, então o saldo tem de voltar ao "+
			"inicial. Foi exatamente isto que falhou em 12/08: Gabinete fechou em 6 "+
			"e Perfume em 4.\nMovimentos: %v", got, saldoInicial, razao.movtos)
	}

	if ativas := reservasAtivasDoEvento(t, fx.eventID); ativas != 0 {
		t.Errorf("%d reservas continuam ativas depois de cancelar tudo", ativas)
	}
}

// A corrida do incidente, agora ponta a ponta: dois requests concorrentes
// reduzindo o mesmo item não podem devolver mais unidades do que foram tiradas.
func TestCicloCompletoComReducoesConcorrentesNaoInventaUnidade(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const saldoInicial = 5
	razao := novoRazao(map[string]int{productID: saldoInicial})

	cartID, _ := seedReservaAtiva(t, fx, productID, 2)
	razao.saida(productID, 2)

	// PATCH, DELETE e a retentativa do comprador — as TRÊS tentativas de 12/08
	// (17:21:51, 17:21:52 e 17:21:59). Só duas podem ser aplicadas: a reserva
	// tinha 2 unidades. A terceira é a que virava o movimento órfão.
	var wg sync.WaitGroup
	largada := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			out, err := repo.DecrementActiveReservationQuantity(ctx, cartID, productID, 1)
			if err != nil {
				t.Errorf("redução concorrente: %v", err)
				return
			}
			// O movimento no ERP só sai quando o banco confirmou a baixa. É a
			// ordem que a correção instituiu, e é o que impede o órfão.
			if out.Applied {
				razao.entrada(productID, 1)
			}
		}()
	}
	close(largada)
	wg.Wait()

	if got := razao.ler(productID); got != saldoInicial {
		t.Errorf("saldo no Tiny = %d, quero %d — duas reduções de 1 sobre uma reserva "+
			"de 2 devolvem exatamente 2. Mais que isso é a unidade fantasma do "+
			"movimento 365095970", got, saldoInicial)
	}
	if ativas := reservasAtivasDoCarrinho(t, cartID, productID); ativas != 0 {
		t.Errorf("reserva ainda segura %d depois de zerada", ativas)
	}
}

func reservasAtivasDoCarrinho(t *testing.T, cartID, productID string) int {
	t.Helper()
	var total *int
	if err := testPool.QueryRow(context.Background(),
		`SELECT sum(quantity)::int FROM stock_reservations
		 WHERE cart_id = $1::uuid AND product_id = $2::uuid AND status = 'active'`,
		cartID, productID).Scan(&total); err != nil {
		t.Fatalf("somando reservas ativas: %v", err)
	}
	if total == nil {
		return 0
	}
	return *total
}

func reservasAtivasDoEvento(t *testing.T, eventID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM stock_reservations WHERE event_id = $1::uuid AND status = 'active'`,
		eventID).Scan(&n); err != nil {
		t.Fatalf("contando reservas ativas: %v", err)
	}
	return n
}
