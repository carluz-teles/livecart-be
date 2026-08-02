package migrationtest

// Teste da migration 000106: cupom uma vez por COMPRADOR na campanha (RN-33).
//
// O caso que a 000105 criou: pagar abre um 2º carrinho no mesmo evento, e a
// UNIQUE (cart_id) sozinha deixaria o mesmo cupom ser resgatado de novo.

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000106CouponPerBuyer(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()
	if err := m.Migrate(106); err != nil {
		t.Fatalf("migrate ate 106: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID, eventID, couponID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Cupom Test','cupom-106') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'active','Semana Black') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO coupons (event_id, code, type, percent_bps)
		 VALUES ($1,'BLACK20','percent',2000) RETURNING id::text`,
		eventID,
	).Scan(&couponID); err != nil {
		t.Fatalf("seed coupon: %v", err)
	}

	// Dois carrinhos do MESMO comprador no mesmo evento — legal desde a 000105.
	newCart := func(seq int, status, payment string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
			 VALUES ($1,'maria','maria',$2,$3,$4,$5) RETURNING id::text`,
			eventID, fmt.Sprintf("tok-106-%d", seq), seq, status, payment,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %d: %v", seq, err)
		}
		return id
	}
	redeem := func(cartID, status string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO coupon_redemptions (coupon_id, cart_id, platform_user_id, status, applied_value_cents)
			 SELECT $1, $2, c.platform_user_id, $3, 1000 FROM carts c WHERE c.id = $2::uuid`,
			couponID, cartID, status,
		)
		return err
	}

	cart1 := newCart(1, "checkout", "paid")
	cart2 := newCart(2, "active", "pending")

	if err := redeem(cart1, "confirmed"); err != nil {
		t.Fatalf("primeiro resgate devia entrar: %v", err)
	}
	if err := redeem(cart2, "reserved"); err == nil {
		t.Error("a mesma compradora resgatou o cupom num 2o carrinho — o guard por comprador nao pegou")
	}

	// Um comprador diferente continua podendo usar.
	var outro string
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1,'joao','joao','tok-106-9',9,'active','pending') RETURNING id::text`,
		eventID,
	).Scan(&outro); err != nil {
		t.Fatalf("seed cart joao: %v", err)
	}
	if err := redeem(outro, "reserved"); err != nil {
		t.Errorf("outro comprador devia poder usar o cupom: %v", err)
	}

	// Resgate expirado/estornado libera a vaga: o comprador nunca ficou com o
	// benefício, então pode usar de novo no ciclo seguinte.
	for _, st := range []string{"expired", "refunded"} {
		t.Run(st+" libera a vaga", func(t *testing.T) {
			if _, err := pool.Exec(ctx,
				`UPDATE coupon_redemptions SET status=$1 WHERE cart_id=$2::uuid`, st, cart1,
			); err != nil {
				t.Fatalf("mudar status: %v", err)
			}
			if err := redeem(cart2, "reserved"); err != nil {
				t.Errorf("status %q devia liberar a vaga: %v", st, err)
			}
			// devolve o estado para o próximo subteste
			if _, err := pool.Exec(ctx, `DELETE FROM coupon_redemptions WHERE cart_id=$1::uuid`, cart2); err != nil {
				t.Fatalf("limpar: %v", err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE coupon_redemptions SET status='confirmed' WHERE cart_id=$1::uuid`, cart1,
			); err != nil {
				t.Fatalf("restaurar: %v", err)
			}
		})
	}
}
