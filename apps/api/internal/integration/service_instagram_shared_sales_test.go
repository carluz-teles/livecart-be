//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/payment"
)

type sharedInstagramStock struct{ calls []sharedInstagramSale }
type sharedInstagramSale struct{ storeID, productID, erpID string }

func (s *sharedInstagramStock) NoteReserved(context.Context, live.ReserveParams) error { return nil }
func (s *sharedInstagramStock) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, handle string) error {
	row, err := testRepo.GetActiveERP(ctx, storeID)
	if err != nil {
		return err
	}
	s.calls = append(s.calls, sharedInstagramSale{storeID, productID, row.ID})
	return nil
}

type sharedInstagramPayment struct{ providers.PaymentProvider }

func TestInstagramSharedSalesRemainStoreScoped(t *testing.T) {
	requireDB(t)
	for _, kind := range []string{"live", "post", "reel", "story"} {
		t.Run(kind, func(t *testing.T) { testSharedInstagramSales(t, kind) })
	}
}

func testSharedInstagramSales(t *testing.T, kind string) {
	t.Helper()
	ctx := t.Context()
	a, b := seedScaleEvent(t), seedScaleEvent(t)
	account := "shared-sales-" + a.storeID
	stock := &sharedInstagramStock{}
	svc := live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	svc.SetIngestRepository(liveIngestRepoAdapter{testRepo})
	svc.SetStockReserver(stock)
	integrations := reconnectTestService(t)
	integrations.SetPaymentService(payment.NewService(integrations, nil, zap.NewNop()))
	var paymentConfigs []providers.PagarmeConfig
	integrations.factory = providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(), PagarmeConstructor: func(cfg providers.PagarmeConfig) (providers.PaymentProvider, error) {
			paymentConfigs = append(paymentConfigs, cfg)
			return &sharedInstagramPayment{}, nil
		},
	})
	var firstMedia, firstPayment string
	for index, fx := range []scaleFixture{a, b} {
		provider := []string{"tiny", "bling"}[index]
		seedSharedInstagram(t, fx.storeID, account, "scoped-"+account, "active")
		erpRow, err := testRepo.Create(ctx, CreateIntegrationParams{StoreID: fx.storeID, Type: "erp", Provider: provider, Status: "active", Credentials: []byte("erp-" + fx.storeID)})
		if err != nil {
			t.Fatal(err)
		}
		creds, err := integrations.encryptor.EncryptJSON(&providers.Credentials{APIKey: "payment-" + fx.storeID})
		if err != nil {
			t.Fatal(err)
		}
		payment, err := testRepo.Create(ctx, CreateIntegrationParams{StoreID: fx.storeID, Type: "payment", Provider: "pagarme", Status: "active", Credentials: creds})
		if err != nil {
			t.Fatal(err)
		}
		product := seedSoldOutProductWithQueue(t, fx, 10, 0)
		price := int64((index + 1) * 1000)
		if _, err := testPool.Exec(ctx, `UPDATE products SET keyword='1234',external_source=$2,price=$3 WHERE id=$1`, product, provider, price); err != nil {
			t.Fatal(err)
		}
		var session string
		if err := testPool.QueryRow(ctx, `INSERT INTO live_sessions(event_id,status,type,sequence_order) VALUES($1,'live',$2,1) RETURNING id::text`, fx.eventID, kind).Scan(&session); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO session_products(session_id,product_id,max_quantity) VALUES($1,$2,10)`, session, product); err != nil {
			t.Fatal(err)
		}
		media := "shared-media-" + session
		if _, err := testPool.Exec(ctx, `INSERT INTO live_session_platforms(session_id,platform,platform_live_id) VALUES($1,'instagram',$2)`, session, media); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstMedia, firstPayment = media, payment.ID
		} else {
			if _, err := svc.CreateSession(ctx, live.CreateSessionInput{StoreID: fx.storeID, EventID: fx.eventID, Type: kind, Platform: "instagram", PlatformLiveID: firstMedia}); err == nil {
				t.Fatal("same media assigned to both businesses")
			}
			if _, err := integrations.GetPaymentProvider(ctx, firstPayment, fx.storeID); err == nil {
				t.Fatal("another store's payment integration was accepted")
			}
		}
		input := live.ProcessInstagramCommentInput{AccountID: account, MediaID: media, CommentID: "shared-comment-" + session, UserID: "same-buyer", Username: "buyer", Text: "Eu quero 1234", Timestamp: time.Now().Unix()}
		if kind == "story" {
			input.Channel = "dm"
		}
		for replay := 0; replay < 2; replay++ {
			if err := svc.ProcessInstagramComment(ctx, input); err != nil {
				t.Fatal(err)
			}
		}
		var cartStore, cartProduct string
		var quantity int
		var actualPrice int64
		if err := testPool.QueryRow(ctx, `SELECT c.store_id::text,ci.product_id::text,ci.quantity,ci.unit_price FROM carts c JOIN cart_items ci ON ci.cart_id=c.id WHERE c.event_id=$1 AND c.platform_user_id='same-buyer'`, fx.eventID).Scan(&cartStore, &cartProduct, &quantity, &actualPrice); err != nil {
			t.Fatal(err)
		}
		if cartStore != fx.storeID || cartProduct != product || quantity != 1 || actualPrice != price {
			t.Fatalf("purchase mixed across stores: %s %s %d %d", cartStore, cartProduct, quantity, actualPrice)
		}
		if len(stock.calls) != index+1 || stock.calls[index] != (sharedInstagramSale{fx.storeID, product, erpRow.ID}) {
			t.Fatalf("wrong ERP reservation: %+v", stock.calls)
		}
		_, resolved, err := integrations.ResolvePaymentProvider(ctx, cartStore, "pagarme")
		if err != nil || resolved != payment.ID {
			t.Fatalf("wrong payment integration: %s %v", resolved, err)
		}
		cfg := paymentConfigs[len(paymentConfigs)-1]
		if cfg.StoreID != fx.storeID || cfg.IntegrationID != payment.ID || cfg.Credentials.APIKey != "payment-"+fx.storeID {
			t.Fatal("payment credentials crossed stores")
		}
	}
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM carts WHERE event_id IN ($1,$2) AND platform_user_id='same-buyer'`, a.eventID, b.eventID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("expected two independent carts; count=%d error=%v", count, err)
	}
}
