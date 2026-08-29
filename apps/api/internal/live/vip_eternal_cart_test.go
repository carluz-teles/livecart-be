package live

// Clientes VIP: carrinho ETERNO cross-evento (20/08/2026).
//
// O carrinho do VIP não expira e acumula itens de eventos diferentes no MESMO
// carrinho. Estes testes provam, contra o Postgres real:
//   1. compra no evento X e depois no evento Y caem no mesmo carrinho eterno;
//   2. fechar o evento X NÃO arma expiração no carrinho eterno;
//   3. um comprador NÃO-VIP continua com um carrinho por evento (regressão).

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedEventInStore cria um evento numa loja específica (para dois eventos da
// MESMA loja compartilharem o carrinho VIP).
func seedEventInStore(t *testing.T, storeID string) string {
	t.Helper()
	var eventID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','Ev '||$2,now()+interval '7 days') RETURNING id::text`,
		storeID, fmt.Sprintf("%d", time.Now().UnixNano()),
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

func seedStore(t *testing.T) string {
	t.Helper()
	var storeID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('VIP Store','vip-'||$1) RETURNING id::text`,
		fmt.Sprintf("%d", time.Now().UnixNano()),
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return storeID
}

func getOrCreateVip(t *testing.T, storeID, eventID, buyer string) (*CartRow, bool) {
	t.Helper()
	cart, created, err := testRepo.GetOrCreateCart(context.Background(), GetOrCreateCartParams{
		EventID:        eventID,
		StoreID:        storeID,
		PlatformUserID: buyer,
		PlatformHandle: buyer,
		Token:          fmt.Sprintf("tok-%d", time.Now().UnixNano()),
		IsVip:          true,
	})
	if err != nil {
		t.Fatalf("GetOrCreateCart VIP: %v", err)
	}
	return cart, created
}

func TestVipCartIsSharedAcrossEvents(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	eventX := seedEventInStore(t, storeID)
	eventY := seedEventInStore(t, storeID)

	// Compra no evento X → cria o carrinho eterno.
	cartX, createdX := getOrCreateVip(t, storeID, eventX, "alisson")
	if !createdX {
		t.Fatal("primeira compra VIP devia criar o carrinho")
	}
	addItem(t, cartX.ID, 1, 3000)

	// Evento X fecha; o carrinho do Alisson NÃO expira (é eterno).
	if _, err := testPool.Exec(context.Background(),
		`UPDATE live_events SET status='ended', ends_at=now()-interval '1 hour' WHERE id=$1`, eventX); err != nil {
		t.Fatalf("encerrar evento X: %v", err)
	}

	// Nova compra no evento Y → tem de cair no MESMO carrinho.
	cartY, createdY := getOrCreateVip(t, storeID, eventY, "alisson")
	if createdY {
		t.Fatal("compra no evento Y criou carrinho novo — o VIP deveria reusar o eterno")
	}
	if cartY.ID != cartX.ID {
		t.Fatalf("carrinho do evento Y (%s) != carrinho do evento X (%s) — não é o mesmo", cartY.ID, cartX.ID)
	}

	// never_expires e store_id gravados; expires_at NULL.
	var neverExpires bool
	var expiresAt *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT never_expires, expires_at FROM carts WHERE id=$1::uuid`, cartX.ID).
		Scan(&neverExpires, &expiresAt); err != nil {
		t.Fatalf("ler cart: %v", err)
	}
	if !neverExpires {
		t.Error("carrinho VIP não está marcado never_expires")
	}
	if expiresAt != nil {
		t.Errorf("carrinho VIP ganhou expires_at %v — não pode expirar", expiresAt)
	}
}

func TestFinalizeSkipsEternalCart(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	eventID := seedEventInStore(t, storeID)

	// Um VIP e um comprador normal no mesmo evento.
	vipCart, _ := getOrCreateVip(t, storeID, eventID, "vipbuyer")
	normalCart, _, nErr := testRepo.GetOrCreateCart(context.Background(), GetOrCreateCartParams{
		EventID: eventID, StoreID: storeID, PlatformUserID: "normalbuyer",
		PlatformHandle: "normalbuyer", Token: fmt.Sprintf("t-%d", time.Now().UnixNano()),
	})
	if nErr != nil {
		t.Fatalf("criar carrinho normal: %v", nErr)
	}

	// Fecha o evento.
	if _, err := testRepo.FinalizeCartsByEvent(context.Background(), eventID); err != nil {
		t.Fatalf("FinalizeCartsByEvent: %v", err)
	}

	read := func(id string) (status string, exp *time.Time) {
		if err := testPool.QueryRow(context.Background(),
			`SELECT status, expires_at FROM carts WHERE id=$1::uuid`, id).Scan(&status, &exp); err != nil {
			t.Fatalf("ler cart %s: %v", id, err)
		}
		return
	}

	// Normal: virou checkout com prazo.
	nStatus, nExp := read(normalCart.ID)
	if nStatus != "checkout" || nExp == nil {
		t.Errorf("carrinho normal = (%s, %v); esperava checkout com expires_at", nStatus, nExp)
	}
	// VIP: intocado, sem prazo.
	vStatus, vExp := read(vipCart.ID)
	if vStatus == "checkout" || vExp != nil {
		t.Errorf("carrinho eterno foi finalizado (%s, %v) — deveria ficar intocado", vStatus, vExp)
	}
}

func TestNonVipStillGetsCartPerEvent(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	eventX := seedEventInStore(t, storeID)
	eventY := seedEventInStore(t, storeID)

	cartX, _, xErr := testRepo.GetOrCreateCart(context.Background(), GetOrCreateCartParams{
		EventID: eventX, StoreID: storeID, PlatformUserID: "joana",
		PlatformHandle: "joana", Token: fmt.Sprintf("t-%d", time.Now().UnixNano()),
	})
	if xErr != nil {
		t.Fatalf("carrinho evento X: %v", xErr)
	}
	cartY, createdY, yErr := testRepo.GetOrCreateCart(context.Background(), GetOrCreateCartParams{
		EventID: eventY, StoreID: storeID, PlatformUserID: "joana",
		PlatformHandle: "joana", Token: fmt.Sprintf("t-%d", time.Now().UnixNano()),
	})
	if yErr != nil {
		t.Fatalf("carrinho evento Y: %v", yErr)
	}
	if !createdY {
		t.Fatal("comprador NÃO-VIP deveria ganhar um carrinho novo por evento")
	}
	if cartY.ID == cartX.ID {
		t.Fatal("carrinhos de eventos diferentes de um não-VIP não podem ser o mesmo")
	}
}

// Ao promover um @ a VIP, os carrinhos abertos que ele JÁ tem viram eternos e
// a agenda de expiração é anulada (predicado de ActivateEternalCartsForHandle).
func TestActivateEternalCartsAnnulsExpiry(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	eventID := seedEventInStore(t, storeID)

	var cartID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id,
		     status, payment_status, expires_at)
		 VALUES ($1,$2,'u-vip','carla','tok-'||$3,(floor(random()*90000)+10000)::int,
		     'checkout','pending', now()+interval '1 hour') RETURNING id::text`,
		eventID, storeID, fmt.Sprintf("%d", time.Now().UnixNano()),
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	tag, err := testPool.Exec(context.Background(),
		`UPDATE carts SET never_expires=true, expires_at=NULL
		 WHERE store_id=$1::uuid AND platform_handle=$2
		   AND status IN ('pending','active','checkout')
		   AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))`,
		storeID, "carla")
	if err != nil {
		t.Fatalf("activate eternal: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("carrinhos ativados = %d; esperava 1", tag.RowsAffected())
	}

	var neverExpires bool
	var expiresAt *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT never_expires, expires_at FROM carts WHERE id=$1::uuid`, cartID).
		Scan(&neverExpires, &expiresAt); err != nil {
		t.Fatalf("ler cart: %v", err)
	}
	if !neverExpires {
		t.Error("carrinho não virou eterno após promoção a VIP")
	}
	if expiresAt != nil {
		t.Errorf("agenda de expiração não foi anulada: expires_at=%v", expiresAt)
	}
}

// ─── Juntar compras num pedido pago (26/08/2026) ────────────────────────────
//
// "Carrinho com pagamento APROVADO também devemos juntar. Se foi faturado aí sim
// não pode mais juntar pedidos."
//
// O pedido pago segue aberto até virar nota: a compradora pagou na live de
// segunda, pede mais uma coisa na quinta, e sai uma caixa só. Depois do
// faturamento a nota existe, e somar item nela seria emitir nota errada — então
// a compra de quinta abre um pedido NOVO.
//
// O ERP não impõe esse limite (em 26/08/2026 aceitou, com 204, editar os itens
// de um pedido "Faturada"). Quem recusa é esta query.

// marcarPago encena o pagamento no carrinho, como as queries de pagamento fazem.
func marcarPago(t *testing.T, cartID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET payment_status='paid', paid_at=now(), expires_at=NULL WHERE id=$1`,
		cartID); err != nil {
		t.Fatalf("marcando pago: %v", err)
	}
}

func marcarSituacaoERP(t *testing.T, cartID, situacao string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET erp_order_status=$2, erp_order_status_at=now(), external_order_id='ext-1' WHERE id=$1`,
		cartID, situacao); err != nil {
		t.Fatalf("marcando situação: %v", err)
	}
}

func TestVipCartPagoAindaRecebeCompraDeOutroEvento(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	evSegunda := seedEventInStore(t, storeID)
	evQuinta := seedEventInStore(t, storeID)
	comprador := fmt.Sprintf("@maria%d", time.Now().UnixNano())

	segunda, _ := getOrCreateVip(t, storeID, evSegunda, comprador)
	marcarPago(t, segunda.ID)
	marcarSituacaoERP(t, segunda.ID, "aprovado")

	quinta, criou := getOrCreateVip(t, storeID, evQuinta, comprador)
	if criou {
		t.Error("abriu carrinho novo para uma compra que devia entrar no pedido pago")
	}
	if quinta.ID != segunda.ID {
		t.Errorf("a compra de quinta caiu em %s e o pedido pago é %s — o lojista "+
			"queria uma caixa só", quinta.ID, segunda.ID)
	}
}

func TestVipCartFaturadoNaoRecebeMaisCompra(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	evSegunda := seedEventInStore(t, storeID)
	evQuinta := seedEventInStore(t, storeID)
	comprador := fmt.Sprintf("@ana%d", time.Now().UnixNano())

	segunda, _ := getOrCreateVip(t, storeID, evSegunda, comprador)
	marcarPago(t, segunda.ID)
	marcarSituacaoERP(t, segunda.ID, "faturado")

	quinta, criou := getOrCreateVip(t, storeID, evQuinta, comprador)
	if !criou {
		t.Error("não abriu carrinho novo depois da nota emitida")
	}
	if quinta.ID == segunda.ID {
		t.Error("somou item num pedido já faturado — seria emitir nota errada")
	}
}

// Toda situação pós-faturamento fecha a porta; as anteriores não.
func TestPortaDeJuncaoPorSituacaoDoERP(t *testing.T) {
	requireDB(t)
	casos := []struct {
		situacao string
		junta    bool
	}{
		{"aberto", true},
		{"aprovado", true},
		{"preparando_envio", false}, // a nota já saiu quando o pedido entra em preparo
		{"dados_incompletos", true},
		{"faturado", false},
		{"pronto_envio", false},
		{"enviado", false},
		{"entregue", false},
		{"nao_entregue", false},
		{"cancelado", false},
	}
	for _, c := range casos {
		t.Run(c.situacao, func(t *testing.T) {
			requireDB(t)
			storeID := seedStore(t)
			ev1 := seedEventInStore(t, storeID)
			ev2 := seedEventInStore(t, storeID)
			comprador := fmt.Sprintf("@c%s%d", c.situacao, time.Now().UnixNano())

			primeiro, _ := getOrCreateVip(t, storeID, ev1, comprador)
			marcarPago(t, primeiro.ID)
			marcarSituacaoERP(t, primeiro.ID, c.situacao)

			segundo, _ := getOrCreateVip(t, storeID, ev2, comprador)
			juntou := segundo.ID == primeiro.ID
			if juntou != c.junta {
				verbo := map[bool]string{true: "juntou", false: "não juntou"}
				t.Errorf("situação %q: %s, e a regra diz que devia %s",
					c.situacao, verbo[juntou], verbo[c.junta])
			}
		})
	}
}

// Estornado é o oposto do pago: não há venda a que somar, e o carrinho novo
// nasce limpo.
func TestVipCartEstornadoNaoRecebeMaisCompra(t *testing.T) {
	requireDB(t)
	storeID := seedStore(t)
	ev1 := seedEventInStore(t, storeID)
	ev2 := seedEventInStore(t, storeID)
	comprador := fmt.Sprintf("@bia%d", time.Now().UnixNano())

	primeiro, _ := getOrCreateVip(t, storeID, ev1, comprador)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET payment_status='refunded' WHERE id=$1`, primeiro.ID); err != nil {
		t.Fatalf("estornando: %v", err)
	}

	segundo, criou := getOrCreateVip(t, storeID, ev2, comprador)
	if !criou || segundo.ID == primeiro.ID {
		t.Error("juntou compra num carrinho estornado")
	}
}
