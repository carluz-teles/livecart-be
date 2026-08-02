package order_test

// A metade CONFIRMADA do invariante da Fatia 5, provada sobre o selamento REAL.
//
// A pergunta que o épico promete responder é "quanto a live de terça faturou".
// Ela só tem resposta confiável se a soma das transmissões reconstruir, ao
// centavo, o total do pedido — e o total do pedido é congelado no pagamento
// (orders.total_cents), não recalculado.
//
// Aqui o carrinho é montado com o MESMO produto vindo de duas transmissões
// (o caso normal do carrinho unificado da campanha, não a exceção), o pedido é
// selado pelo OnCartPaid de produção, e o que se assere é a igualdade entre:
//
//	SUM(receita por sessão) == orders.total_cents == GetEventStats.confirmed_revenue
//
// Uma alocação que "quase" fecha (arredondando, subtraindo waitlist, ou
// escondendo o balde sem-transmissão) falha aqui.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func parseUUIDT(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parseUUIDT(%q): %v", s, err)
	}
	return u
}

type twoSessionSeed struct {
	storeID   string
	eventID   string
	cartID    string
	segunda   string
	quarta    string
	unitPrice int64
	// gmv é o total do carrinho pela fórmula canônica.
	gmv int64
}

// seedCartFromTwoSessions monta um carrinho pago com 3 unidades do mesmo
// produto: 1 adicionada na transmissão de segunda e 2 na de quarta, mais um
// brinde SEM transmissão (o item posto pelo painel).
func seedCartFromTwoSessions(t *testing.T) twoSessionSeed {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Loja Fatia5', 'f5-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1, 'ended', 'Semana Black') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	session := func(seq int) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO live_sessions (event_id, status, sequence_order)
			 VALUES ($1::uuid, 'ended', $2) RETURNING id::text`, eventID, seq,
		).Scan(&id); err != nil {
			t.Fatalf("seed session %d: %v", seq, err)
		}
		return id
	}
	segunda, quarta := session(1), session(2)

	product := func(name string, price int64, salt int64) string {
		var id string
		kw := fmt.Sprintf("%04d", (time.Now().UnixNano()+salt)%9000+1000)
		if err := testPool.QueryRow(ctx,
			`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
			 VALUES ($1, $2, 'none', 'ext-'||$3||'-'||$4, $5, $6, 100) RETURNING id::text`,
			storeID, name, n, fmt.Sprintf("%d", salt), kw, price,
		).Scan(&id); err != nil {
			t.Fatalf("seed product %s: %v", name, err)
		}
		return id
	}
	const unitPrice int64 = 2500
	const brindePrice int64 = 700
	vestido := product("Vestido", unitPrice, 0)
	brinde := product("Brinde", brindePrice, 1)

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents)
		 VALUES ($1::uuid, $2::uuid, 'u-'||$3, '@b'||$3, 'tok-'||$3, ($3)::bigint % 100000,
		   'checkout', 'paid', now(), 0, 0) RETURNING id::text`,
		// session_id do CARRINHO aponta para a segunda de propósito: é o campo
		// que a métrica antiga usava, e o teste tem de continuar certo mesmo
		// quando ele mente sobre onde a venda aconteceu.
		eventID, segunda, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	items := []struct {
		product string
		qty     int32
		price   int64
	}{
		{vestido, 3, unitPrice},
		{brinde, 1, brindePrice},
	}
	var gmv int64
	for _, it := range items {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 0, $5::uuid)`,
			cartID, it.product, it.qty, it.price, segunda,
		); err != nil {
			t.Fatalf("seed cart_items: %v", err)
		}
		gmv += int64(it.qty) * it.price
	}

	// O log de adições: 1 vestido na segunda, 2 na quarta. O brinde entrou pelo
	// painel, sem transmissão — e não gera linha de log nenhuma.
	logAdd := func(product, sessionID string, qty int32, price int64) {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
			 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)`,
			cartID, product, sessionID, qty, price,
		); err != nil {
			t.Fatalf("seed cart_item_events: %v", err)
		}
	}
	logAdd(vestido, segunda, 1, unitPrice)
	logAdd(vestido, quarta, 2, unitPrice)

	return twoSessionSeed{
		storeID: storeID, eventID: eventID, cartID: cartID,
		segunda: segunda, quarta: quarta, unitPrice: unitPrice, gmv: gmv,
	}
}

func TestSessionConfirmedRevenueMatchesOrderTotal(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	seed := seedCartFromTwoSessions(t)

	// O selamento de produção.
	if err := newListener(t).OnCartPaid(ctx, seed.cartID, seed.storeID, seed.gmv, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	rows, err := testQueries.ListSessionConfirmedRevenueByEvent(ctx, parseUUIDT(t, seed.eventID))
	if err != nil {
		t.Fatalf("ListSessionConfirmedRevenueByEvent: %v", err)
	}

	var soma int64
	porSessao := map[string]int64{}
	for _, r := range rows {
		soma += r.RevenueCents
		id := "" // balde sem transmissão
		if r.SessionID.Valid {
			id = r.SessionID.String()
		}
		porSessao[id] += r.RevenueCents
	}

	// A quebra: 1 vestido na segunda, 2 na quarta, o brinde sem transmissão.
	if got := porSessao[seed.segunda]; got != seed.unitPrice {
		t.Errorf("segunda = %d, quero %d (1 vestido)", got, seed.unitPrice)
	}
	if got := porSessao[seed.quarta]; got != 2*seed.unitPrice {
		t.Errorf("quarta = %d, quero %d (2 vestidos)", got, 2*seed.unitPrice)
	}
	if got := porSessao[""]; got != 700 {
		t.Errorf("sem transmissão = %d, quero 700 (o brinde do painel)", got)
	}

	// O invariante, contra o congelado do pedido.
	var totalCents int64
	if err := testPool.QueryRow(ctx,
		`SELECT total_cents FROM orders WHERE cart_id = $1::uuid`, seed.cartID,
	).Scan(&totalCents); err != nil {
		t.Fatalf("ler orders.total_cents: %v", err)
	}
	if soma != totalCents {
		t.Errorf("soma das sessões (%d) != orders.total_cents (%d)", soma, totalCents)
	}

	// E contra o número que a tela do evento mostra.
	stats, err := testQueries.GetEventStats(ctx, parseUUIDT(t, seed.eventID))
	if err != nil {
		t.Fatalf("GetEventStats: %v", err)
	}
	if soma != stats.ConfirmedRevenue {
		t.Errorf("soma das sessões (%d) != confirmed_revenue do evento (%d)", soma, stats.ConfirmedRevenue)
	}
}

func TestSessionConfirmedRevenueIsNotFirstTouch(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	seed := seedCartFromTwoSessions(t)
	if err := newListener(t).OnCartPaid(ctx, seed.cartID, seed.storeID, seed.gmv, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	rows, err := testQueries.ListSessionConfirmedRevenueByEvent(ctx, parseUUIDT(t, seed.eventID))
	if err != nil {
		t.Fatalf("ListSessionConfirmedRevenueByEvent: %v", err)
	}

	var naSegunda int64
	for _, r := range rows {
		if r.SessionID.Valid && r.SessionID.String() == seed.segunda {
			naSegunda = r.RevenueCents
		}
	}

	// carts.session_id e cart_items.session_id apontam AMBOS para a segunda
	// (first-touch). Se a métrica lesse qualquer um dos dois, a segunda
	// levaria o carrinho inteiro. Ela lê o log.
	if naSegunda == seed.gmv {
		t.Errorf("a segunda levou o carrinho inteiro (%d) — a atribuição voltou a ser first-touch", naSegunda)
	}
	if naSegunda != seed.unitPrice {
		t.Errorf("segunda = %d, quero %d", naSegunda, seed.unitPrice)
	}
}
