//go:build integration

package checkout

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/crypto"
)

type stockReadDuringCheckout struct {
	providers.ERPProvider
	read func() error
}

type checkoutGridERP struct {
	providers.ERPProvider
	mu            sync.Mutex
	grid          []providers.ERPOrderItem
	reads, writes int
	beforeRead    func()
}

func (*checkoutGridERP) Name() providers.ProviderName { return providers.ProviderTiny }
func (p *checkoutGridERP) GetOrderItems(context.Context, string) ([]providers.ERPOrderItem, error) {
	if p.beforeRead != nil {
		p.beforeRead()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	return append([]providers.ERPOrderItem{}, p.grid...), nil
}
func (p *checkoutGridERP) UpdateOrderItems(_ context.Context, _ string, grid []providers.ERPOrderItem) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes++
	p.grid = append([]providers.ERPOrderItem{}, grid...)
	return nil
}

func wireCheckoutERP(t *testing.T, f editFixture, provider providers.ERPProvider) *integration.Service {
	t.Helper()
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := enc.EncryptJSON(providers.Credentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET credentials=$2 WHERE store_id=$1`, f.store, credentials); err != nil {
		t.Fatal(err)
	}
	factory := providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) { return provider, nil }})
	svc := integration.NewService(integration.NewRepository(testQueries, testPool), factory, enc, nil, nil, zap.NewNop())
	f.service.integrationService = svc
	f.service.merchantEditERP = svc
	return svc
}

func TestCartEdit_ManualLineAfterLastERPRemovalUsesRealLifecycle(t *testing.T) {
	for _, batched := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_confirmation", true: "same_pending_batch"}[batched], func(t *testing.T) {
			f := seedMerchantEdit(t)
			fake := &checkoutGridERP{grid: []providers.ERPOrderItem{{ProductID: "1234", Quantity: 2, UnitPrice: 1000}}}
			wireCheckoutERP(t, f, fake)
			var product, item string
			if err := testPool.QueryRow(t.Context(), `INSERT INTO products(store_id,name,keyword,price,stock,external_source) VALUES($1,'Manual','MAN1',1000,8,'manual') RETURNING id::text`, f.store).Scan(&product); err != nil {
				t.Fatal(err)
			}
			if err := testPool.QueryRow(t.Context(), `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price) VALUES($1,$2,2,1000) RETURNING id::text`, f.cart, product).Scan(&item); err != nil {
				t.Fatal(err)
			}
			if err := f.service.RemoveCartItemAsMerchant(t.Context(), f.token, f.item); err != nil {
				t.Fatal(err)
			}
			if !batched {
				dueMerchantEdit(t, f.cart)
				f.service.RecoverMerchantEdits(t.Context())
			}
			if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), f.token, item, 1); err != nil {
				t.Fatal(err)
			}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			if err := cartedit.AssertReady(t.Context(), testPool, f.cart); err != nil {
				t.Fatal(err)
			}
			if len(fake.grid) != 0 || fake.writes != 1 {
				t.Fatalf("ERP removal lost/repeated: grid=%+v writes=%d", fake.grid, fake.writes)
			}
			assertEditStock(t, f, 10)
			f.product = product
			assertEditStock(t, f, 9)
			var pending bool
			if err := testPool.QueryRow(t.Context(), `SELECT erp_pending_since IS NOT NULL FROM cart_items WHERE id=$1`, item).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending {
				t.Fatal("manual marker remains pending")
			}
		})
	}
}

func TestCartEdit_SweepAndWorkerExcludeEachOtherDuringERP(t *testing.T) {
	for _, sweepFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker_first", true: "sweep_first"}[sweepFirst], func(t *testing.T) {
			f := seedMerchantEdit(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			fake := &checkoutGridERP{grid: []providers.ERPOrderItem{{ProductID: "1234", Quantity: 2, UnitPrice: 1000}}, beforeRead: func() { once.Do(func() { close(entered); <-resume }) }}
			svc := wireCheckoutERP(t, f, fake)
			if err := f.service.SetCartItemQuantityAsMerchant(t.Context(), f.token, f.item, 1); err != nil {
				t.Fatal(err)
			}
			if sweepFirst {
				if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_state='mutating',erp_op_resting_state='open',erp_op_started_at=now()-interval '4 minutes' WHERE id=$1`, f.cart); err != nil {
					t.Fatal(err)
				}
			}
			dueMerchantEdit(t, f.cart)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if sweepFirst {
					svc.RunERPOrderOpsSweep(t.Context())
				} else {
					f.service.RecoverMerchantEdits(t.Context())
				}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				close(resume)
				t.Fatal("did not enter ERP")
			}
			if !sweepFirst {
				if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_op_started_at=now()-interval '4 minutes' WHERE id=$1`, f.cart); err != nil {
					close(resume)
					t.Fatal(err)
				}
				svc.RunERPOrderOpsSweep(t.Context())
			} else {
				f.service.RecoverMerchantEdits(t.Context())
			}
			close(resume)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("ERP operation failed to finish")
			}
			dueMerchantEdit(t, f.cart)
			f.service.RecoverMerchantEdits(t.Context())
			if err := cartedit.AssertReady(t.Context(), testPool, f.cart); err != nil {
				t.Fatal(err)
			}
			if fake.writes != 1 {
				t.Fatalf("competing writers: %d", fake.writes)
			}
			assertEditStock(t, f, 9)
		})
	}
}

func (p stockReadDuringCheckout) Name() providers.ProviderName { return providers.ProviderTiny }

func (p stockReadDuringCheckout) GetOrderItems(context.Context, string) ([]providers.ERPOrderItem, error) {
	return nil, p.read()
}

func TestSynchronousCheckoutStockCannotReofferReservedUnit(t *testing.T) {
	for _, operation := range []string{"quantity", "addition"} {
		for _, merchant := range []bool{false, true} {
			name := operation + "/buyer"
			if merchant {
				name = operation + "/merchant"
			}
			t.Run(name, func(t *testing.T) { testSynchronousCheckoutStock(t, operation, merchant) })
		}
	}
}

func testSynchronousCheckoutStock(t *testing.T, operation string, merchant bool) {
	t.Helper()
	f := seedMerchantEdit(t)
	ctx := t.Context()
	if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_status='aberto' WHERE id=$1`, f.cart); err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := enc.EncryptJSON(providers.Credentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE integrations SET credentials=$2 WHERE store_id=$1`, f.store, credentials); err != nil {
		t.Fatal(err)
	}
	repo := integration.NewRepository(testQueries, testPool)
	var svc *integration.Service
	observed, beforeMirror, afterMirror := false, 0, 0
	fake := stockReadDuringCheckout{read: func() error {
		observed = true
		var seq int64
		if err := testPool.QueryRow(ctx, `SELECT erp_seq,stock FROM products WHERE id=$1`, f.product).Scan(&seq, &beforeMirror); err != nil {
			return err
		}
		// Tiny still has the old order (two units): eight units available.
		// Exercise the actual compensation and CAS used by stock webhooks.
		available := svc.PortaoAPartirDoSaldoDoERP(ctx, &integration.IntegrationRow{StoreID: f.store, Provider: "tiny"}, "1234", 8)
		_, _ = repo.ApplyERPStockMirror(ctx, f.product, available, seq) // pending edits deliberately defer this read
		if err := testPool.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1`, f.product).Scan(&afterMirror); err != nil {
			return err
		}
		return errors.New("ERP unavailable before applying the edit")
	}}
	factory := providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return fake, nil
	}})
	svc = integration.NewService(repo, factory, enc, nil, nil, zap.NewNop())
	f.service.integrationService = svc
	f.service.merchantEditERP = svc
	var editErr error
	if operation == "addition" {
		_, editErr = f.service.AddCartItem(ctx, MutateCartItemInput{Token: f.token, ProductID: f.product, Quantity: 1, ByMerchant: merchant})
	} else {
		_, editErr = f.service.UpdateCartItemQuantity(ctx, MutateCartItemInput{Token: f.token, ItemID: f.item, Quantity: 3, ByMerchant: merchant})
	}
	if !observed {
		t.Fatalf("did not reach ERP read: %v", editErr)
	}
	if editErr != nil {
		t.Fatalf("accepted edit returned failure: %v", editErr)
	}
	status, err := cartedit.Read(ctx, testPool, f.cart)
	if err != nil || !status.Pending || status.LastError == "" {
		t.Fatalf("missing pending status: %+v %v", status, err)
	}
	var stock, quantity int
	if err := testPool.QueryRow(ctx, `SELECT p.stock,ci.quantity FROM products p JOIN cart_items ci ON ci.product_id=p.id WHERE ci.id=$1`, f.item).Scan(&stock, &quantity); err != nil {
		t.Fatal(err)
	}
	if beforeMirror != 7 || afterMirror > 7 || stock != 7 || quantity != 3 {
		t.Fatalf("before mirror=%d after mirror=%d after ERP failure=%d quantity=%d; want 7/<=7/7/3", beforeMirror, afterMirror, stock, quantity)
	}
}
