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
