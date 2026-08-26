package integration

// Promoção a VIP com mais de um carrinho aberto (26/08/2026).
//
// O modelo admite UM carrinho eterno por comprador — carts_one_eternal_per_store_buyer,
// índice único parcial em (store_id, platform_handle). Mas quem chega à promoção
// com dois carrinhos abertos é o caso normal: antes de virar VIP, o comprador
// ganha um carrinho por evento.
//
// A versão antiga marcava never_expires nos DOIS num update só, violava o índice
// e o Postgres desfazia tudo — nenhum carrinho virava eterno, e o erro morria no
// best-effort do chamador. Em produção isso deixou @eulalisueli com R$ 2.480,80
// em dois carrinhos, o mais antigo a horas de expirar, e a tela dizendo
// "o carrinho dele nunca vai expirar".
//
// Estes testes rodam contra o Postgres real (TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type vipFixture struct {
	storeID  string
	handle   string
	eventA   string // evento mais antigo
	eventB   string // evento mais recente
	productA string
	productB string
}

// seedVipBuyer cria loja, dois eventos e dois produtos para um mesmo @.
func seedVipBuyer(t *testing.T) vipFixture {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var fx vipFixture
	fx.handle = "buyer" + n
	mustScan := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustScan(&fx.storeID,
		`INSERT INTO stores (name, slug) VALUES ('Loja VIP','loja-vip-'||$1) RETURNING id::text`, n)
	mustScan(&fx.eventA,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'ended','Evento A '||$2, now()) RETURNING id::text`, fx.storeID, n)
	mustScan(&fx.eventB,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','Evento B '||$2, now()+interval '2 days') RETURNING id::text`, fx.storeID, n)
	mustScan(&fx.productA,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Produto A','tiny','EXTA-'||$2,$3,1000,10) RETURNING id::text`,
		fx.storeID, n, fmt.Sprintf("%d", 1000+seedSeq%4000))
	mustScan(&fx.productB,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Produto B','tiny','EXTB-'||$2,$3,2000,10) RETURNING id::text`,
		fx.storeID, n, fmt.Sprintf("%d", 5000+seedSeq%4000))
	return fx
}

// seedOpenCart cria um carrinho aberto do @ num evento, com um item e uma reserva.
// createdAgo posiciona o carrinho no tempo — é o critério que elege o sobrevivente.
func seedOpenCart(t *testing.T, fx vipFixture, eventID, productID string, qty int, createdAgo time.Duration, status string) string {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id,
		                    status, expires_at, created_at)
		 VALUES ($1,$2,'user-'||$3,$4,'tok-'||$3,($3)::bigint % 100000,$5, now()+interval '1 day', now()-$6::interval)
		 RETURNING id::text`,
		eventID, fx.storeID, n, fx.handle, status, createdAgo.String()).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1,$2,$3,1000,0)`, cartID, productID, qty); err != nil {
		t.Fatalf("seed cart_items: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_item_events (cart_id, product_id, quantity, unit_price)
		 VALUES ($1,$2,$3,1000)`, cartID, productID, qty); err != nil {
		t.Fatalf("seed cart_item_events: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id, status)
		 VALUES ($1,$2,$3,'EXT-'||$4,$5,'MOV-'||$4,'active')`,
		eventID, cartID, productID, n, qty); err != nil {
		t.Fatalf("seed stock_reservations: %v", err)
	}
	return cartID
}

func cartState(t *testing.T, cartID string) (status string, neverExpires bool, hasExpiry bool, reason string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, never_expires, expires_at IS NOT NULL, COALESCE(cancelled_reason,'')
		 FROM carts WHERE id = $1`, cartID).Scan(&status, &neverExpires, &hasExpiry, &reason); err != nil {
		t.Fatalf("lendo carrinho: %v", err)
	}
	return
}

func cartItemQty(t *testing.T, cartID, productID string) int {
	t.Helper()
	var q int
	err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(quantity),0)::int FROM cart_items WHERE cart_id=$1 AND product_id=$2`,
		cartID, productID).Scan(&q)
	if err != nil {
		t.Fatalf("lendo item: %v", err)
	}
	return q
}

func countBy(t *testing.T, table, cartID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*)::int FROM %s WHERE cart_id=$1`, table), cartID).Scan(&n); err != nil {
		t.Fatalf("contando %s: %v", table, err)
	}
	return n
}

// O caso da produção: dois carrinhos abertos, nenhum produto em comum.
func TestPromocaoVipConsolidaDoisCarrinhosAbertos(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	antigo := seedOpenCart(t, fx, fx.eventA, fx.productA, 8, 10*24*time.Hour, "checkout")
	recente := seedOpenCart(t, fx, fx.eventB, fx.productB, 54, 3*24*time.Hour, "active")

	res, err := testRepo.ConsolidateEternalCartForHandle(context.Background(), fx.storeID, fx.handle)
	if err != nil {
		t.Fatalf("consolidação falhou: %v", err)
	}

	if res.EternalCartID != recente {
		t.Fatalf("o eterno tem de ser o carrinho MAIS RECENTE (o que o resolvedor acha): quis %s, veio %s", recente, res.EternalCartID)
	}
	if len(res.MergedCartIDs) != 1 || res.MergedCartIDs[0] != antigo {
		t.Fatalf("esperava o carrinho antigo fundido, veio %v", res.MergedCartIDs)
	}

	status, eterno, temPrazo, _ := cartState(t, recente)
	if !eterno || temPrazo || status != "active" {
		t.Fatalf("o sobrevivente devia ficar eterno e sem prazo: status=%s eterno=%v temPrazo=%v", status, eterno, temPrazo)
	}
	status, _, _, motivo := cartState(t, antigo)
	if status != "cancelled" || motivo != "merged_into_vip_cart" {
		t.Fatalf("o fundido devia sair de cena como fundido: status=%s motivo=%s", status, motivo)
	}

	// O conteúdo inteiro está no sobrevivente — nada ficou para trás.
	if q := cartItemQty(t, recente, fx.productA); q != 8 {
		t.Errorf("item do carrinho antigo devia ter migrado: quis 8, veio %d", q)
	}
	if q := cartItemQty(t, recente, fx.productB); q != 54 {
		t.Errorf("item do sobrevivente devia continuar: quis 54, veio %d", q)
	}
	if n := countBy(t, "cart_items", antigo); n != 0 {
		t.Errorf("carrinho fundido devia ficar vazio, tem %d itens", n)
	}
	// O log de adições é o que diz de qual evento veio cada unidade: sem ele no
	// sobrevivente, a métrica por evento de origem perde as peças migradas.
	if n := countBy(t, "cart_item_events", recente); n != 2 {
		t.Errorf("log de adições devia ter seguido os itens: quis 2, veio %d", n)
	}
	// A reserva acompanha a peça — senão o estoque seria devolvido por engano.
	if n := countBy(t, "stock_reservations", recente); n != 2 {
		t.Errorf("reservas deviam ter seguido os itens: quis 2, veio %d", n)
	}
}

// Mesmo produto nos dois carrinhos: soma, e não estoura o índice de cart_items.
func TestPromocaoVipSomaProdutoRepetido(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	antigo := seedOpenCart(t, fx, fx.eventA, fx.productA, 3, 10*24*time.Hour, "checkout")
	recente := seedOpenCart(t, fx, fx.eventB, fx.productA, 2, 3*24*time.Hour, "active")

	if _, err := testRepo.ConsolidateEternalCartForHandle(context.Background(), fx.storeID, fx.handle); err != nil {
		t.Fatalf("consolidação falhou: %v", err)
	}
	if q := cartItemQty(t, recente, fx.productA); q != 5 {
		t.Fatalf("o mesmo produto nos dois devia somar: quis 5, veio %d", q)
	}
	if n := countBy(t, "cart_items", antigo); n != 0 {
		t.Errorf("carrinho fundido devia ficar vazio, tem %d itens", n)
	}
}

// Regressão: com um carrinho só, a promoção continua sendo o que sempre foi.
func TestPromocaoVipComUmCarrinhoSoSegueIgual(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	unico := seedOpenCart(t, fx, fx.eventB, fx.productA, 4, time.Hour, "active")

	res, err := testRepo.ConsolidateEternalCartForHandle(context.Background(), fx.storeID, fx.handle)
	if err != nil {
		t.Fatalf("consolidação falhou: %v", err)
	}
	if res.EternalCartID != unico || len(res.MergedCartIDs) != 0 {
		t.Fatalf("um carrinho só: nada a fundir. veio eterno=%s fundidos=%v", res.EternalCartID, res.MergedCartIDs)
	}
	if _, eterno, temPrazo, _ := cartState(t, unico); !eterno || temPrazo {
		t.Fatalf("devia ter virado eterno e perdido o prazo")
	}
}

// Sem carrinho aberto a promoção é válida e não faz nada.
func TestPromocaoVipSemCarrinhoAberto(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)

	res, err := testRepo.ConsolidateEternalCartForHandle(context.Background(), fx.storeID, fx.handle)
	if err != nil {
		t.Fatalf("consolidação falhou: %v", err)
	}
	if res.EternalCartID != "" || len(res.MergedCartIDs) != 0 {
		t.Fatalf("nada a consolidar, mas veio %+v", res)
	}
}

// Carrinho com pedido no ERP não é esvaziado às cegas: fica de fora e é
// devolvido ao chamador. Esvaziá-lo deixaria um pedido lá dentro segurando peça.
func TestPromocaoVipPulaCarrinhoComPedidoNoERP(t *testing.T) {
	requireDB(t)
	fx := seedVipBuyer(t)
	comPedido := seedOpenCart(t, fx, fx.eventA, fx.productA, 3, 10*24*time.Hour, "checkout")
	recente := seedOpenCart(t, fx, fx.eventB, fx.productB, 2, 3*24*time.Hour, "active")

	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET external_order_id='TINY-123', erp_order_state='open' WHERE id=$1`, comPedido); err != nil {
		t.Fatalf("marcando pedido no ERP: %v", err)
	}

	res, err := testRepo.ConsolidateEternalCartForHandle(context.Background(), fx.storeID, fx.handle)
	if err != nil {
		t.Fatalf("consolidação falhou: %v", err)
	}
	if res.EternalCartID != recente {
		t.Fatalf("o mais recente devia virar eterno, veio %s", res.EternalCartID)
	}
	if len(res.SkippedCartIDs) != 1 || res.SkippedCartIDs[0] != comPedido {
		t.Fatalf("o carrinho com pedido no ERP devia ser reportado, veio %v", res.SkippedCartIDs)
	}
	if status, _, _, _ := cartState(t, comPedido); status != "checkout" {
		t.Fatalf("o carrinho pulado devia continuar vivo, veio %s", status)
	}
	if q := cartItemQty(t, comPedido, fx.productA); q != 3 {
		t.Fatalf("o carrinho pulado devia manter os itens, veio %d", q)
	}
}
