package exporter

// Fatia 4/6: prova a query FindCommentCartCorrelation (db/queries/live_comment.sql)
// contra Postgres real — o JOIN por event_id+platform_user_id e a janela de
// 120s (cart_item_events.created_at BETWEEN comment.created_at AND +120s) não
// dá para provar com fake, é aritmética de intervalo que só o Postgres real
// valida. Mesmo padrão de apps/api/internal/live/latest_comment_window_test.go.
//
// A correlação casa por cart_item_events.created_at (uma linha por ADIÇÃO ao
// carrinho, RN-12/migration 000110), não por carts.created_at: GetOrCreateCart
// (internal/live/repository.go) reusa o carrinho ABERTO do comprador no
// evento, então só o comentário que CRIA o carrinho tem carts.created_at
// dentro da própria janela — um 2º comentário do mesmo comprador que adiciona
// outro produto ao carrinho já aberto teria cart bem mais velho que o
// comentário. cart_item_events tem created_at próprio por adição, então casa
// nos dois casos.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// nextShortID feeds carts.short_id (NOT NULL, no DB default — see migration
// 000062), which the app normally assigns via a per-store counter. Tests only
// need distinct values, not the real per-store sequencing.
var shortIDSeq int64

func nextShortID() int64 {
	return atomic.AddInt64(&shortIDSeq, 1) + 100000
}

// productSeq feeds products.keyword/external_id (UNIQUE per store) so
// multiple seeded products in the same test don't collide.
var productSeq int64

func nextProductSeq() int64 {
	return atomic.AddInt64(&productSeq, 1)
}

func seedCorrelationEvent(t *testing.T, ctx context.Context, slug string) (eventID, storeID string) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1,$1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1,'active',$2, now() + interval '7 days') RETURNING id::text`,
		storeID, slug,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID, storeID
}

func seedCorrelationComment(t *testing.T, ctx context.Context, eventID, platformCommentID, platformUserID string, ageSeconds int) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO live_comments (event_id, platform, platform_comment_id, platform_user_id, platform_handle, text, has_purchase_intent, matched_product_id, result, created_at)
		 VALUES ($1,'instagram',$2,$3,'@buyer','eu quero',true,NULL,'added_to_cart', now() - make_interval(secs => $4::int))`,
		eventID, platformCommentID, platformUserID, ageSeconds,
	); err != nil {
		t.Fatalf("seed comment %s: %v", platformCommentID, err)
	}
}

// seedCorrelationCart inserts a cart for eventID/platformUserID, created
// ageSeconds ago (relative to now). carts.created_at no longer drives the
// correlation match (cart_item_events.created_at does, see
// seedCorrelationCartItem below) — kept realistic anyway since a cart always
// exists before an item lands in it.
func seedCorrelationCart(t *testing.T, ctx context.Context, eventID, platformUserID string, ageSeconds int) (cartID string) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, created_at)
		 VALUES ($1,$2,'@buyer',$3,$4, now() - make_interval(secs => $5::int))
		 RETURNING id::text`,
		eventID, platformUserID, "tok-"+platformUserID, nextShortID(), ageSeconds,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	return cartID
}

// seedCorrelationProduct inserts a minimal product for storeID so
// cart_item_events (product_id NOT NULL FK) can reference it.
func seedCorrelationProduct(t *testing.T, ctx context.Context, storeID string) (productID string) {
	t.Helper()
	seq := nextProductSeq()
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Produto Teste','none',$2,$3,10.0,100) RETURNING id::text`,
		storeID,
		fmt.Sprintf("ext-%d-%d", time.Now().UnixNano(), seq),
		fmt.Sprintf("%04d", seq%10000),
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return productID
}

// seedCorrelationCartItem inserts a cart_item_events row (one line per
// addition, RN-12) for cartID, added ageSeconds ago. This is what
// FindCommentCartCorrelation actually matches against — a cart existing is
// not enough, an item must have been added inside the comment's window.
func seedCorrelationCartItem(t *testing.T, ctx context.Context, cartID, productID string, ageSeconds int) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_item_events (cart_id, product_id, quantity, unit_price, created_at)
		 VALUES ($1,$2,1,1000, now() - make_interval(secs => $3::int))`,
		cartID, productID, ageSeconds,
	); err != nil {
		t.Fatalf("seed cart_item_event: %v", err)
	}
}

// seedCorrelationCartItemAt is like seedCorrelationCartItem but takes an
// explicit created_at instead of "ageSeconds ago now()". Boundary tests need
// this: computing "comment_created_at + exactly 120s" from two independent
// now() calls (one per INSERT statement) is off by however many milliseconds
// elapse between the statements, which is enough to flip a boundary check.
func seedCorrelationCartItemAt(t *testing.T, ctx context.Context, cartID, productID string, at time.Time) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_item_events (cart_id, product_id, quantity, unit_price, created_at)
		 VALUES ($1,$2,1,1000,$3)`,
		cartID, productID, at,
	); err != nil {
		t.Fatalf("seed cart_item_event at %v: %v", at, err)
	}
}

// getCommentCreatedAt reads back the created_at Postgres actually stored for
// a seeded comment, so boundary tests can derive an item's timestamp as an
// exact offset from it instead of racing two independent now() calls.
func getCommentCreatedAt(t *testing.T, ctx context.Context, platformCommentID string) time.Time {
	t.Helper()
	var createdAt time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT created_at FROM live_comments WHERE platform_comment_id = $1`,
		platformCommentID,
	).Scan(&createdAt); err != nil {
		t.Fatalf("read back comment created_at: %v", err)
	}
	return createdAt
}

func TestFindCommentCartCorrelation(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	t.Run("acha o carrinho quando o item foi adicionado dentro da janela de 120s", func(t *testing.T) {
		eventID, storeID := seedCorrelationEvent(t, ctx, "corr-in-window")
		seedCorrelationComment(t, ctx, eventID, "c-in-window", "ig-buyer-1", 90)
		cartID := seedCorrelationCart(t, ctx, eventID, "ig-buyer-1", 5)
		productID := seedCorrelationProduct(t, ctx, storeID)
		seedCorrelationCartItem(t, ctx, cartID, productID, 5)

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-in-window")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation: %v", err)
		}
		if !row.CartID.Valid {
			t.Fatal("cart_id veio NULL, queria o carrinho cujo item caiu na janela")
		}
		if got := uuidString(row.CartID); got != cartID {
			t.Errorf("cart_id = %q, want %q", got, cartID)
		}
		if !row.HasPurchaseIntent.Bool {
			t.Error("has_purchase_intent = false, want true (seedado true)")
		}
		if row.Result.String != "added_to_cart" {
			t.Errorf("result = %q, want added_to_cart", row.Result.String)
		}
	})

	t.Run("ignora item adicionado fora da janela de 120s", func(t *testing.T) {
		eventID, storeID := seedCorrelationEvent(t, ctx, "corr-out-window")
		seedCorrelationComment(t, ctx, eventID, "c-out-window", "ig-buyer-2", 300)
		cartID := seedCorrelationCart(t, ctx, eventID, "ig-buyer-2", 100)
		productID := seedCorrelationProduct(t, ctx, storeID)
		// Comentário há 300s, item adicionado há 100s: o item entrou 200s DEPOIS
		// do comentário — fora da janela [comment, comment+120s].
		seedCorrelationCartItem(t, ctx, cartID, productID, 100)

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-out-window")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation: %v", err)
		}
		if row.CartID.Valid {
			t.Errorf("cart_id = %q, queria NULL (item fora da janela)", uuidString(row.CartID))
		}
	})

	t.Run("sem nenhum carrinho para o comprador/evento", func(t *testing.T) {
		eventID, _ := seedCorrelationEvent(t, ctx, "corr-none")
		seedCorrelationComment(t, ctx, eventID, "c-none", "ig-buyer-3", 10)

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-none")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation: %v", err)
		}
		if row.CartID.Valid {
			t.Error("cart_id veio preenchido, queria NULL (nenhum carrinho criado)")
		}
		if row.PlatformUserID != "ig-buyer-3" {
			t.Errorf("platform_user_id = %q, want ig-buyer-3", row.PlatformUserID)
		}
	})

	t.Run("carrinho existe mas nenhum item foi adicionado na janela", func(t *testing.T) {
		// Reviewer finding: um carrinho pertencer ao comprador/evento não basta
		// — sem uma linha de cart_item_events dentro da janela, não houve
		// conversão observável e converted_to_cart deve continuar false.
		eventID, _ := seedCorrelationEvent(t, ctx, "corr-cart-no-item")
		seedCorrelationComment(t, ctx, eventID, "c-cart-no-item", "ig-buyer-6", 10)
		seedCorrelationCart(t, ctx, eventID, "ig-buyer-6", 5)

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-cart-no-item")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation: %v", err)
		}
		if row.CartID.Valid {
			t.Errorf("cart_id = %q, queria NULL (carrinho existe mas sem item na janela)", uuidString(row.CartID))
		}
	})

	t.Run("múltiplos comentários no mesmo evento correlacionam cada um com seu próprio carrinho", func(t *testing.T) {
		eventID, storeID := seedCorrelationEvent(t, ctx, "corr-multi")
		seedCorrelationComment(t, ctx, eventID, "c-multi-1", "ig-buyer-4", 200)
		cart1 := seedCorrelationCart(t, ctx, eventID, "ig-buyer-4", 100)
		product1 := seedCorrelationProduct(t, ctx, storeID)
		seedCorrelationCartItem(t, ctx, cart1, product1, 100)

		seedCorrelationComment(t, ctx, eventID, "c-multi-2", "ig-buyer-5", 50)
		cart2 := seedCorrelationCart(t, ctx, eventID, "ig-buyer-5", 10)
		product2 := seedCorrelationProduct(t, ctx, storeID)
		seedCorrelationCartItem(t, ctx, cart2, product2, 10)

		row1, err := testQueries.FindCommentCartCorrelation(ctx, "c-multi-1")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation c-multi-1: %v", err)
		}
		if got := uuidString(row1.CartID); got != cart1 {
			t.Errorf("comment 1: cart_id = %q, want %q", got, cart1)
		}

		row2, err := testQueries.FindCommentCartCorrelation(ctx, "c-multi-2")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation c-multi-2: %v", err)
		}
		if got := uuidString(row2.CartID); got != cart2 {
			t.Errorf("comment 2: cart_id = %q, want %q", got, cart2)
		}
	})

	t.Run("segundo comentário do mesmo comprador reusando carrinho já aberto conta como conversão", func(t *testing.T) {
		// Este é o cenário do finding MEDIUM: GetOrCreateCart reusa o carrinho
		// aberto do comprador no evento, então o 2º comentário (que adiciona um
		// 2º produto) não cria um carrinho novo — o cart existente é bem mais
		// velho que o comentário. Antes do fix, a correlação casava por
		// carts.created_at e reportava converted_to_cart=false para esse
		// comentário, mesmo o item tendo sido adicionado de fato.
		eventID, storeID := seedCorrelationEvent(t, ctx, "corr-reuse")
		buyer := "ig-buyer-reuse"

		// 1º comentário: cria o carrinho e adiciona o produto A, 195s atrás.
		seedCorrelationComment(t, ctx, eventID, "c-reuse-1", buyer, 200)
		cartID := seedCorrelationCart(t, ctx, eventID, buyer, 195)
		productA := seedCorrelationProduct(t, ctx, storeID)
		seedCorrelationCartItem(t, ctx, cartID, productA, 195)

		// 2º comentário do MESMO comprador, 60s atrás: reusa o carrinho já
		// aberto (criado há 195s — fora da janela [60s atrás, 60s à frente] do
		// 2º comentário) e adiciona o produto B, 55s atrás (dentro da janela).
		seedCorrelationComment(t, ctx, eventID, "c-reuse-2", buyer, 60)
		productB := seedCorrelationProduct(t, ctx, storeID)
		seedCorrelationCartItem(t, ctx, cartID, productB, 55)

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-reuse-2")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation c-reuse-2: %v", err)
		}
		if !row.CartID.Valid {
			t.Fatal("cart_id veio NULL, queria o carrinho reaberto (item B caiu na janela do 2º comentário)")
		}
		if got := uuidString(row.CartID); got != cartID {
			t.Errorf("cart_id = %q, want %q (mesmo carrinho reusado)", got, cartID)
		}
		if !row.ItemCreatedAt.Valid {
			t.Fatal("item_created_at veio NULL, queria o timestamp da adição do produto B")
		}
	})

	t.Run("boundary: item adicionado exatamente aos 120s ainda conta como dentro da janela", func(t *testing.T) {
		eventID, storeID := seedCorrelationEvent(t, ctx, "corr-boundary")
		buyer := "ig-buyer-boundary"
		seedCorrelationComment(t, ctx, eventID, "c-boundary", buyer, 130)
		commentCreatedAt := getCommentCreatedAt(t, ctx, "c-boundary")

		cartID := seedCorrelationCart(t, ctx, eventID, buyer, 130)
		productID := seedCorrelationProduct(t, ctx, storeID)
		// Item adicionado EXATAMENTE 120s depois do comentário — o limite
		// superior da janela. BETWEEN é inclusivo nos dois extremos, então
		// isto deve casar.
		seedCorrelationCartItemAt(t, ctx, cartID, productID, commentCreatedAt.Add(120*time.Second))

		row, err := testQueries.FindCommentCartCorrelation(ctx, "c-boundary")
		if err != nil {
			t.Fatalf("FindCommentCartCorrelation: %v", err)
		}
		if !row.CartID.Valid {
			t.Fatal("cart_id veio NULL, queria match no limite exato dos 120s (BETWEEN é inclusivo)")
		}
		if got := uuidString(row.CartID); got != cartID {
			t.Errorf("cart_id = %q, want %q", got, cartID)
		}
	})
}
