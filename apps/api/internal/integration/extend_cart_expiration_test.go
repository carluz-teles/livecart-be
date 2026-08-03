package integration

// Promover alguém da fila NÃO pode criar prazo num carrinho que não tinha.
//
// O caso real, em staging: o comprador entrou na fila, o produto liberou às
// 14:10 e a promoção gravou um prazo de 30 minutos no CARRINHO. Ele nunca soube
// (a DM da promoção falhou), seguiu comprando — adicionou outro produto às
// 14:39 — e às 14:40 perdeu o carrinho inteiro. O evento estava aberto até dois
// dias depois.
//
// A causa era ler NULL como "sem prazo" quando NULL é a RN-04: "não expira
// enquanto o evento roda", o prazo mais LONGO que existe. O COALESCE trocava
// NULL pelo valor novo e o GREATEST então comparava o valor novo com ele mesmo.
//
// A regra que o dono do produto definiu é dar tempo A MAIS a quem esperou.
// Encurtar é o oposto, e é o pior encurtamento possível: silencioso, no
// comprador que já tinha sido prejudicado uma vez pela falta de estoque.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// uuidParam converte o id textual do seed no tipo que o sqlc espera.
func uuidParam(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	var out pgtype.UUID
	if err := out.Scan(id); err != nil {
		t.Fatalf("uuid %q: %v", id, err)
	}
	return out
}

// tsParam embrulha o carimbo no tipo do sqlc.
func tsParam(v time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: v, Valid: true}
}

func TestExtendCartExpirationNuncaCriaPrazoOndeNaoHavia(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	q := sqlc.New(testPool)

	n := fmt.Sprintf("%d", time.Now().UnixNano())
	var storeID, eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Ext Store', 'ext-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','Semana Black', now() + interval '2 days') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	newCart := func(suffix string, expiresAt *time.Time) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, expires_at)
			 VALUES ($1,'u-'||$2,'@'||$2,'tok-'||$2||'-'||$3, (('x'||md5($2||$3))::bit(20)::int), 'active', $4)
			 RETURNING id::text`,
			eventID, suffix, n, expiresAt,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %s: %v", suffix, err)
		}
		return id
	}

	readExpiry := func(cartID string) *time.Time {
		t.Helper()
		var out *time.Time
		if err := testPool.QueryRow(ctx,
			`SELECT expires_at FROM carts WHERE id = $1::uuid`, cartID,
		).Scan(&out); err != nil {
			t.Fatalf("ler expires_at: %v", err)
		}
		return out
	}

	// O TTL da fila, como a promoção o calcula: agora + 30 min.
	ttl := time.Now().UTC().Add(30 * time.Minute)

	t.Run("carrinho sem prazo continua sem prazo", func(t *testing.T) {
		cartID := newCart("sem-prazo", nil)

		if err := q.ExtendCartExpiration(ctx, sqlc.ExtendCartExpirationParams{
			ID: uuidParam(t, cartID), NewExpiresAt: tsParam(ttl),
		}); err != nil {
			t.Fatalf("ExtendCartExpiration: %v", err)
		}

		if got := readExpiry(cartID); got != nil {
			t.Errorf("a promocao criou um prazo (%s) num carrinho que ia ate o fim do evento", got)
		}
	})

	t.Run("prazo mais curto e empurrado para frente", func(t *testing.T) {
		curto := time.Now().UTC().Add(5 * time.Minute)
		cartID := newCart("prazo-curto", &curto)

		if err := q.ExtendCartExpiration(ctx, sqlc.ExtendCartExpirationParams{
			ID: uuidParam(t, cartID), NewExpiresAt: tsParam(ttl),
		}); err != nil {
			t.Fatalf("ExtendCartExpiration: %v", err)
		}

		got := readExpiry(cartID)
		if got == nil {
			t.Fatal("o prazo sumiu — a extensao nao pode apagar um prazo existente")
		}
		if got.Before(ttl.Add(-time.Second)) {
			t.Errorf("prazo ficou em %s, esperado ao menos %s", got, ttl)
		}
	})

	t.Run("prazo mais longo nao encolhe", func(t *testing.T) {
		longo := time.Now().UTC().Add(6 * time.Hour)
		cartID := newCart("prazo-longo", &longo)

		if err := q.ExtendCartExpiration(ctx, sqlc.ExtendCartExpirationParams{
			ID: uuidParam(t, cartID), NewExpiresAt: tsParam(ttl),
		}); err != nil {
			t.Fatalf("ExtendCartExpiration: %v", err)
		}

		got := readExpiry(cartID)
		if got == nil || got.Before(longo.Add(-time.Second)) {
			t.Errorf("prazo encolheu para %v — GREATEST existe para impedir isso", got)
		}
	})
}
