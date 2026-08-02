package live

// RN-08: carrinho expirado + comprador volta = carrinho NOVO, antigo arquivado.
//
// Antes da 000107 a unique era total, então GetOrCreateCart não conseguia criar
// um 2º carrinho e reabria o morto in-place — APAGANDO os itens do comprador
// (DeleteCartItemsByCart). Numa campanha de uma semana isso apagaria a compra de
// dias. Estes testes provam que nada mais é destruído.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func seedEvent(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var storeID, eventID string
	slug := fmt.Sprintf("live-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Live Test', $1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1,'active','Semana Teste', now() + interval '7 days') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

// addItem coloca um produto no carrinho, para provar que ele sobrevive.
func addItem(t *testing.T, cartID string, qty int32, price int64) {
	t.Helper()
	ctx := context.Background()
	var storeID string
	if err := testPool.QueryRow(ctx,
		`SELECT e.store_id::text FROM carts c JOIN live_events e ON e.id = c.event_id WHERE c.id = $1::uuid`,
		cartID,
	).Scan(&storeID); err != nil {
		t.Fatalf("resolver store: %v", err)
	}
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Vestido','none',$2,$3,$4,100) RETURNING id::text`,
		storeID,
		fmt.Sprintf("ext-%d", time.Now().UnixNano()),
		fmt.Sprintf("%04d", time.Now().UnixNano()%10000),
		price,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price) VALUES ($1::uuid,$2::uuid,$3,$4)`,
		cartID, productID, qty, price,
	); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}
}

func itemCount(t *testing.T, cartID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cart_items WHERE cart_id = $1::uuid`, cartID,
	).Scan(&n); err != nil {
		t.Fatalf("contar itens: %v", err)
	}
	return n
}

func getOrCreate(t *testing.T, eventID, buyer string) (*CartRow, bool) {
	t.Helper()
	cart, created, err := testRepo.GetOrCreateCart(context.Background(), GetOrCreateCartParams{
		EventID:        eventID,
		PlatformUserID: buyer,
		PlatformHandle: buyer,
		Token:          fmt.Sprintf("tok-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("GetOrCreateCart: %v", err)
	}
	return cart, created
}

func TestGetOrCreateCartReusesOpenCart(t *testing.T) {
	requireDB(t)
	eventID := seedEvent(t)

	first, _ := getOrCreate(t, eventID, "maria")
	addItem(t, first.ID, 1, 2500)

	// Comentar de novo na mesma campanha tem de cair no MESMO carrinho — é a
	// unificação por evento (RN-02).
	second, created := getOrCreate(t, eventID, "maria")
	if second.ID != first.ID {
		t.Errorf("carrinho aberto nao foi reusado: %s != %s", second.ID, first.ID)
	}
	if created {
		t.Error("created=true para carrinho que ja existia")
	}
	if got := itemCount(t, first.ID); got != 1 {
		t.Errorf("itens = %d, quero 1 — o reuso nao pode mexer nos itens", got)
	}
}

func TestGetOrCreateCartOpensNewAfterTerminalState(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		update string
	}{
		{"expirado", `UPDATE carts SET status='expired' WHERE id=$1::uuid`},
		{"cancelado", `UPDATE carts SET status='cancelled' WHERE id=$1::uuid`},
		{"pago", `UPDATE carts SET payment_status='paid', paid_at=now() WHERE id=$1::uuid`},
		{"estornado", `UPDATE carts SET payment_status='refunded' WHERE id=$1::uuid`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eventID := seedEvent(t)
			old, _ := getOrCreate(t, eventID, "joao")
			addItem(t, old.ID, 2, 4000)

			if _, err := testPool.Exec(ctx, tc.update, old.ID); err != nil {
				t.Fatalf("aplicar estado %q: %v", tc.name, err)
			}

			fresh, created := getOrCreate(t, eventID, "joao")

			if fresh.ID == old.ID {
				t.Fatalf("estado %q devia abrir carrinho NOVO, reusou o antigo", tc.name)
			}
			if !created {
				t.Errorf("created=false, esperava carrinho novo")
			}
			// O ponto central da RN-08: o antigo continua inteiro.
			if got := itemCount(t, old.ID); got != 1 {
				t.Errorf("carrinho antigo ficou com %d itens — o reopen destrutivo voltou", got)
			}
			if got := itemCount(t, fresh.ID); got != 0 {
				t.Errorf("carrinho novo nasceu com %d itens, devia nascer limpo", got)
			}
		})
	}
}

// Dois carrinhos abertos ao mesmo tempo continuam impossíveis: o segundo
// GetOrCreateCart tem de achar o primeiro, não estourar na unique.
func TestGetOrCreateCartNeverOpensTwoAtOnce(t *testing.T) {
	requireDB(t)
	eventID := seedEvent(t)

	a, _ := getOrCreate(t, eventID, "ana")
	b, _ := getOrCreate(t, eventID, "ana")
	c, _ := getOrCreate(t, eventID, "ana")

	if a.ID != b.ID || b.ID != c.ID {
		t.Errorf("tres chamadas deviam devolver o mesmo carrinho aberto: %s / %s / %s", a.ID, b.ID, c.ID)
	}

	var abertos int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM carts
		 WHERE event_id = $1::uuid AND platform_user_id = 'ana'
		   AND status IN ('pending','active','checkout')
		   AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))`,
		eventID,
	).Scan(&abertos); err != nil {
		t.Fatalf("contar abertos: %v", err)
	}
	if abertos != 1 {
		t.Errorf("carrinhos abertos = %d, quero exatamente 1", abertos)
	}
}
