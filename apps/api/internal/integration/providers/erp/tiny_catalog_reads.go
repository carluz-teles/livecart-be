package erp

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"

	"livecart/apps/api/lib/ratelimit"
)

// Providers are rebuilt per request. Share only overlapping interactive
// catalogue GETs in this process, scoped to the tenant, integration and token.
// No completed response is cached: a new import rechecks active state/price,
// and /estoque always has its own fresh read, even for simultaneous imports.
var tinyCatalogReads = struct {
	sync.Mutex
	flights map[tinyCatalogReadKey]*tinyCatalogRead
}{flights: make(map[tinyCatalogReadKey]*tinyCatalogRead)}

type tinyCatalogReadKey struct {
	storeID, integrationID, endpoint string
	token                            [sha256.Size]byte
}

type tinyCatalogRead struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	resp    *http.Response
	body    []byte
	err     error
}

// readCatalog is deliberately used only by ListProducts and getProduct, before
// decoding the payload. Each caller owns its product slices/maps and validates
// stock separately. Order reads, stock reads and writes never enter this path.
func (t *Tiny) readCatalog(ctx context.Context, endpoint string) (*http.Response, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	unscoped := t.StoreID == "" || t.IntegrationID == ""
	if unscoped || !ratelimit.IsTinyInteractiveRead(ctx) {
		return t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	}
	key := tinyCatalogReadKey{
		storeID: t.StoreID, integrationID: t.IntegrationID, endpoint: endpoint,
		token: sha256.Sum256([]byte(t.credentials.AccessToken)),
	}
	tinyCatalogReads.Lock()
	flight := tinyCatalogReads.flights[key]
	if flight == nil {
		// Keep the existing independent budgets: the limiter bounds admission,
		// then HTTPClient.Timeout bounds HTTP. Each caller retains its deadline;
		// the last waiter leaving cancels shared work in either phase.
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &tinyCatalogRead{done: make(chan struct{}), cancel: cancel}
		tinyCatalogReads.flights[key] = flight
		go func() {
			defer cancel()
			flight.resp, flight.body, flight.err = t.DoRequest(shared, http.MethodGet, endpoint, nil, t.authHeaders())
			tinyCatalogReads.Lock()
			if tinyCatalogReads.flights[key] == flight {
				delete(tinyCatalogReads.flights, key)
			}
			close(flight.done)
			tinyCatalogReads.Unlock()
		}()
	}
	flight.waiters++
	tinyCatalogReads.Unlock()
	defer func() {
		tinyCatalogReads.Lock()
		flight.waiters--
		if flight.waiters == 0 {
			if tinyCatalogReads.flights[key] == flight {
				delete(tinyCatalogReads.flights, key)
			}
			flight.cancel()
		}
		tinyCatalogReads.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return flight.resp, flight.body, flight.err
	}
}
