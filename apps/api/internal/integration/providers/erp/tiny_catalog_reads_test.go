package erp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func catalogResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}
}

func catalogTiny(t *testing.T, transport catalogTransport) *Tiny {
	t.Helper()
	p, err := NewTiny(TinyConfig{
		StoreID: t.Name(), IntegrationID: "catalog-test",
		Credentials: &Credentials{AccessToken: "no-network"}, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.HTTPClient.Transport = transport
	return p
}

const catalogListBody = `{"itens":[{"id":123,"sku":"LC-1","situacao":"A"}],"paginacao":{"limit":20,"total":1}}`

func TestTinyCatalogQuotaWaitPreservesHTTPAndCallerBudgets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		httpTime    time.Duration
		callerTime  time.Duration
		wantElapsed time.Duration
		wantError   bool
	}{
		{name: "28s HTTP after quota still fits", httpTime: 28 * time.Second, callerTime: 45 * time.Second,
			wantElapsed: 30500 * time.Millisecond},
		{name: "HTTP retains its 30s cap", httpTime: 31 * time.Second, callerTime: 45 * time.Second,
			wantElapsed: 32500 * time.Millisecond, wantError: true},
		{name: "caller deadline still cancels shared read", httpTime: 28 * time.Second, callerTime: 20 * time.Second,
			wantElapsed: 20 * time.Second, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int64
				p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-time.After(tc.httpTime):
						return catalogResponse(r, http.StatusOK, catalogListBody), nil
					}
				})
				limiter := ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny(t.Name())
				p.RateLimiter = limiter
				ctx, cancel := context.WithTimeout(ratelimit.WithTinyInteractiveRead(t.Context()), tc.callerTime)
				defer cancel()
				if err := limiter.WaitRequest(ctx, http.MethodGet); err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				out, err := p.ListProducts(ctx, ListProductsParams{SKU: "LC-1"})
				elapsed := time.Since(started)
				if (err != nil) != tc.wantError || (err != nil && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("elapsed=%s result=%+v err=%v", elapsed, out, err)
				}
				if elapsed < tc.wantElapsed || elapsed > tc.wantElapsed+10*time.Millisecond || calls.Load() != 1 {
					t.Fatalf("budget or pacing changed: elapsed=%s calls=%d", elapsed, calls.Load())
				}
			})
		})
	}
}

func TestTinyCatalogConcurrentListsShareOneQuotaSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		transport := catalogTransport(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			time.Sleep(time.Second)
			return catalogResponse(r, http.StatusOK, catalogListBody), nil
		})
		limiter := ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny(t.Name())
		ctx := ratelimit.WithTinyInteractiveRead(t.Context())
		results := make(chan *ProductListResult, 20)
		errs := make(chan error, 20)
		for range 20 {
			// Production builds a new provider for every request.
			p := catalogTiny(t, transport)
			p.RateLimiter = limiter
			go func() {
				out, err := p.ListProducts(ctx, ListProductsParams{SKU: "LC-1", PageSize: 20, ActiveOnly: true})
				results <- out
				errs <- err
			}()
		}
		for range 20 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		first := <-results
		first.Products[0].SKU = "caller mutation"
		for range 19 {
			if out := <-results; len(out.Products) != 1 || out.Products[0].SKU != "LC-1" {
				t.Fatalf("shared mutable result: %+v", out)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("20 identical searches used %d HTTP calls", calls.Load())
		}
	})
}

func TestTinyCatalogImportSharesOnlyMetadataWithoutQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var metadata, stock atomic.Int64
		transport := catalogTransport(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/estoque/") {
				n := stock.Add(1)
				if n > 6 {
					return catalogResponse(r, http.StatusOK, `{"saldo":99}`), nil
				}
				return catalogResponse(r, http.StatusOK, fmt.Sprintf(`{"disponivel":%d}`, n)), nil
			}
			read := metadata.Add(1)
			time.Sleep(time.Second)
			if read > 1 {
				return catalogResponse(r, http.StatusOK, `{"id":123,"situacao":"I"}`), nil
			}
			return catalogResponse(r, http.StatusOK,
				`{"id":123,"sku":"LC-1","situacao":"A","estoque":{"quantidade":99}}`), nil
		})
		ctx := ratelimit.WithTinyInteractiveRead(t.Context())
		results := make(chan *ERPProduct, 6)
		errs := make(chan error, 6)
		for range 6 {
			p := catalogTiny(t, transport)
			go func() {
				out, err := p.GetProductForImport(ctx, "123")
				results <- out
				errs <- err
			}()
		}
		for range 6 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		seen := make(map[int]bool)
		for range 6 {
			p := <-results
			if !p.StockKnown || p.Stock < 1 || p.Stock > 6 || seen[p.Stock] {
				t.Fatalf("import reused stock: %+v", p)
			}
			seen[p.Stock] = true
		}
		if metadata.Load() != 1 || stock.Load() != 6 {
			t.Fatalf("metadata=%d stock=%d; want 1+6 calls instead of 6+6", metadata.Load(), stock.Load())
		}
		p, err := catalogTiny(t, transport).GetProductForImport(ctx, "123")
		if err != nil || p.Active || p.StockKnown || p.Stock != 0 || metadata.Load() != 2 || stock.Load() != 7 {
			t.Fatalf("later import trusted previous or physical stock: %+v err=%v", p, err)
		}
	})
}

func TestTinyCatalogParentPreviewDoesNotInheritAnotherReadersStock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var metadata, stock atomic.Int64
		p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/estoque/") {
				stock.Add(1)
				return catalogResponse(r, http.StatusOK, `{"disponivel":2}`), nil
			}
			metadata.Add(1)
			time.Sleep(time.Second)
			return catalogResponse(r, http.StatusOK,
				`{"id":1,"tipo":"V","situacao":"A","variacoes":[{"id":2},{"id":3}]}`), nil
		})
		ctx := ratelimit.WithTinyInteractiveRead(t.Context())
		preview, legacy := make(chan *ERPProduct, 1), make(chan *ERPProduct, 1)
		errs := make(chan error, 2)
		go func() {
			out, err := p.GetProductForImport(ctx, "1")
			preview <- out
			errs <- err
		}()
		go func() {
			out, err := p.GetProduct(ctx, "1")
			legacy <- out
			errs <- err
		}()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		deferred, complete := <-preview, <-legacy
		if len(deferred.Variants) != 2 || len(complete.Variants) != 2 || metadata.Load() != 1 || stock.Load() != 3 {
			t.Fatalf("unexpected parent reads: metadata=%d stock=%d", metadata.Load(), stock.Load())
		}
		for i, variant := range deferred.Variants {
			if variant.StockKnown || variant.Stock != 0 || !complete.Variants[i].StockKnown {
				t.Fatalf("stock leaked between readers: deferred=%+v complete=%+v", variant, complete.Variants[i])
			}
		}
	})
}

func TestTinyCatalogReadIsolation(t *testing.T) {
	for _, dimension := range []string{"store", "integration", "token", "filter", "limit", "background", "unscoped"} {
		t.Run(dimension, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int64
				transport := catalogTransport(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					time.Sleep(time.Second)
					return catalogResponse(r, http.StatusOK, catalogListBody), nil
				})
				first, second := catalogTiny(t, transport), catalogTiny(t, transport)
				ctx := ratelimit.WithTinyInteractiveRead(t.Context())
				secondCtx := ctx
				params := ListProductsParams{SKU: "LC-1", PageSize: 20}
				secondParams := params
				switch dimension {
				case "store":
					second.StoreID = "another-store"
				case "integration":
					second.IntegrationID = "another-integration"
				case "token":
					second.credentials.AccessToken = "rotated-token"
				case "filter":
					secondParams = ListProductsParams{Search: "LC-1", PageSize: 20}
				case "limit":
					secondParams.PageSize = 1
				case "background":
					secondCtx = ratelimit.WithTinyCatalogRead(t.Context())
				case "unscoped":
					first.StoreID, second.StoreID = "", ""
				}
				errs := make(chan error, 2)
				go func() { _, err := first.ListProducts(ctx, params); errs <- err }()
				go func() { _, err := second.ListProducts(secondCtx, secondParams); errs <- err }()
				for range 2 {
					if err := <-errs; err != nil {
						t.Fatal(err)
					}
				}
				if calls.Load() != 2 {
					t.Fatalf("%s crossed sharing boundary: %d calls", dimension, calls.Load())
				}
			})
		})
	}
}

func TestTinyCatalogCallerCancellationDoesNotCancelOtherWaiters(t *testing.T) {
	for _, cancelLeader := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel_leader_%t", cancelLeader), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int64
				release := make(chan struct{})
				p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-release:
						return catalogResponse(r, http.StatusOK, catalogListBody), nil
					}
				})
				ctx := ratelimit.WithTinyInteractiveRead(t.Context())
				leader, cancelFirst := context.WithCancel(ctx)
				defer cancelFirst()
				follower, cancelSecond := context.WithCancel(ctx)
				defer cancelSecond()
				first, second := make(chan error, 1), make(chan error, 1)
				params := ListProductsParams{SKU: "LC-1"}
				go func() { _, err := p.ListProducts(leader, params); first <- err }()
				synctest.Wait()
				go func() { _, err := p.ListProducts(follower, params); second <- err }()
				synctest.Wait()
				cancelled, remaining := first, second
				if cancelLeader {
					cancelFirst()
				} else {
					cancelSecond()
					cancelled, remaining = second, first
				}
				if err := <-cancelled; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				close(release)
				if err := <-remaining; err != nil {
					t.Fatalf("another caller's cancellation poisoned read: %v", err)
				}
				if calls.Load() != 1 {
					t.Fatalf("cancellation duplicated lookup: %d calls", calls.Load())
				}
			})
		})
	}
}

func TestTinyCatalogLastCancellationStopsWorkAndAllowsNewRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		stopped := make(chan struct{})
		p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				close(stopped)
				return nil, r.Context().Err()
			}
			return catalogResponse(r, http.StatusOK, catalogListBody), nil
		})
		ctx, cancel := context.WithCancel(ratelimit.WithTinyInteractiveRead(t.Context()))
		errs := make(chan error, 1)
		params := ListProductsParams{SKU: "LC-1"}
		go func() { _, err := p.ListProducts(ctx, params); errs <- err }()
		synctest.Wait()
		cancel()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-stopped
		if _, err := p.ListProducts(ratelimit.WithTinyInteractiveRead(t.Context()), params); err != nil {
			t.Fatalf("cancelled read retained: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("calls=%d; want a new read after cancellation", calls.Load())
		}
	})
}

func TestTinyCatalogFailureIsNotCached(t *testing.T) {
	var calls int
	p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return catalogResponse(r, http.StatusTooManyRequests, `{}`), nil
		}
		return catalogResponse(r, http.StatusOK, catalogListBody), nil
	})
	ctx := ratelimit.WithTinyInteractiveRead(t.Context())
	params := providers.ListProductsParams{SKU: "LC-1"}
	if _, err := p.ListProducts(ctx, params); err == nil {
		t.Fatal("429 hidden")
	}
	if _, err := p.ListProducts(ctx, params); err != nil || calls != 2 {
		t.Fatalf("failed read retained: calls=%d err=%v", calls, err)
	}
}

func TestTinyCatalogCancellationWhileQueuedDoesNotDispatchOrReserveQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		p := catalogTiny(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return catalogResponse(r, http.StatusOK, catalogListBody), nil
		})
		limiter := ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny(t.Name())
		p.RateLimiter = limiter
		ctx := ratelimit.WithTinyInteractiveRead(t.Context())
		if err := limiter.WaitRequest(ctx, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		queued, cancel := context.WithCancel(ctx)
		params := ListProductsParams{SKU: "LC-1"}
		errs := make(chan error, 2)
		for range 2 {
			go func() { _, err := p.ListProducts(queued, params); errs <- err }()
		}
		synctest.Wait()
		cancel()
		for range 2 {
			if err := <-errs; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		}
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatalf("abandoned reads dispatched %d calls", calls.Load())
		}
		time.Sleep(2500 * time.Millisecond)
		started := time.Now()
		if _, err := p.ListProducts(ctx, params); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 || time.Since(started) != 0 {
			t.Fatalf("abandoned reads consumed next slot: calls=%d wait=%s", calls.Load(), time.Since(started))
		}
	})
}
