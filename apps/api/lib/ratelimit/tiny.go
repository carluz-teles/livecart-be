package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start at 80% of the smallest documented plan (30/minute), even when
// responses have no quota headers. Learn each category's quota independently.
const tinyDefaultIntervalMS int64 = 2500

// Tiny limits reads and writes independently. PostgreSQL coordinates LiveCart
// replicas; other applications on the merchant's account remain outside it.
type Tiny struct {
	account string
	*requestBudget
}

// requestBudget is the shared PostgreSQL/local dispatch clock. Provider adapters
// choose account keys and feedback; claiming a slot has one implementation.
type requestBudget struct {
	pool              *pgxpool.Pool
	defaultIntervalMS int64
	mu                sync.Mutex
	local             map[string]*tinyBudget
	now               func() time.Time
	tinyPriorities    bool
}

type tinyBudget struct {
	interval             time.Duration
	nextAt, blockedUntil time.Time
	interactiveUntil     time.Time
	catalogWaitSince     time.Time
	catalogUntil         time.Time
}

func (m *Manager) SetSharedPool(pool *pgxpool.Pool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pool = pool
}

func (m *Manager) GetOrCreateTiny(account string) *Tiny {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tiny[account]; t != nil {
		return t
	}
	t := &Tiny{account: account, requestBudget: newRequestBudget(m.pool, tinyDefaultIntervalMS)}
	t.tinyPriorities = true
	m.tiny[account] = t
	return t
}

func (t *Tiny) category(method string) string {
	if method == http.MethodGet || method == http.MethodHead {
		return t.account + ":read"
	}
	return t.account + ":write"
}

func (t *Tiny) Wait(ctx context.Context) error { return t.WaitRequest(ctx, http.MethodPost) }
func (t *Tiny) Allow(ctx context.Context) (*Reservation, error) {
	return t.claim(ctx, t.category(http.MethodPost))
}

// BaseProvider uses ObserveResponse: feedback without a method cannot safely
// be attributed to either budget.
func (t *Tiny) UpdateFromHeaders(int, int) {}

func (t *Tiny) WaitRequest(ctx context.Context, method string) error {
	return t.wait(ctx, t.category(method))
}

func (t *requestBudget) wait(ctx context.Context, key string) error {
	priority := t.readPriority(ctx, key)
	if priority == tinyInteractiveRead {
		// An operator can retry a lookup; spending the whole HTTP budget queued
		// behind account traffic leaves no time to read the result.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tinyInteractiveMaxQuotaWait)
		defer cancel()
	}
	for {
		res, err := t.claim(ctx, key)
		if err != nil {
			return err
		}
		if res.Allowed {
			return nil
		}
		wait := res.RetryAfter + time.Millisecond
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
			return ErrNaoDespachado
		}
		if priority != tinyNormalRead {
			// Both classes renew their pending-reader leases. Rechecking also
			// lets a waiting scan take its fair turn during continuous searches.
			wait = min(wait, time.Second)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrNaoDespachado, ctx.Err())
		case <-timer.C:
		}
	}
}

func (t *requestBudget) localBudget(key string) *tinyBudget {
	if t.local[key] == nil {
		t.local[key] = &tinyBudget{interval: time.Duration(t.defaultIntervalMS) * time.Millisecond}
	}
	return t.local[key]
}

// Claim only the current slot. Waiting/cancelled callers never reserve future
// capacity, and they recheck cooldowns announced while they were waiting.
func (t *requestBudget) claim(ctx context.Context, key string) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNaoDespachado, err)
	}
	priority := t.readPriority(ctx, key)
	if t.pool == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNaoDespachado, err)
		}
		return t.localBudget(key).claim(ctx, priority, t.now()), nil
	}
	if _, err := t.pool.Exec(ctx, `INSERT INTO api_rate_budgets(account_key,interval_ms) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, t.defaultIntervalMS); err != nil {
		return nil, fmt.Errorf("reserving API budget: %w", err)
	}
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var budget tinyBudget
	var intervalMS int64
	err = tx.QueryRow(ctx, `SELECT interval_ms,next_at,blocked_until,
 GREATEST(interactive_until,'epoch'::timestamptz),
 GREATEST(catalog_wait_since,'epoch'::timestamptz),GREATEST(catalog_until,'epoch'::timestamptz)
 FROM api_rate_budgets WHERE account_key=$1 FOR UPDATE`, key).
		Scan(&intervalMS, &budget.nextAt, &budget.blockedUntil,
			&budget.interactiveUntil, &budget.catalogWaitSince, &budget.catalogUntil)
	if err != nil {
		return nil, fmt.Errorf("reading API budget: %w", err)
	}
	// Sample the shared clock after the row lock is acquired. Go's local clock
	// must not decide dispatch times for other replicas.
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	budget.interval = time.Duration(intervalMS) * time.Millisecond
	res := budget.claim(ctx, priority, now)
	_, err = tx.Exec(ctx, `UPDATE api_rate_budgets SET next_at=$2,interactive_until=$3,
 catalog_wait_since=$4,catalog_until=$5 WHERE account_key=$1`,
		key, budget.nextAt, budget.interactiveUntil, budget.catalogWaitSince, budget.catalogUntil)
	if err != nil {
		return nil, fmt.Errorf("claiming API budget: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

func (b *tinyBudget) claim(ctx context.Context, priority tinyReadPriority, now time.Time) *Reservation {
	next := *b
	wait := max(next.nextAt.Sub(now), next.blockedUntil.Sub(now), 0)
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
		// A request which cannot be dispatched must not renew priority leases.
		return &Reservation{RetryAfter: wait, Remaining: -1}
	}
	if priority == tinyCatalogRead {
		if !next.catalogUntil.After(now) {
			next.catalogWaitSince = now
		}
		next.catalogUntil = now.Add(tinyInteractiveLease)
	}
	catalogDue := next.catalogUntil.After(now) && !now.Before(next.catalogWaitSince.Add(tinyCatalogMaxPriorityWait))
	switch priority {
	case tinyInteractiveRead:
		next.interactiveUntil = now.Add(tinyInteractiveLease)
		if catalogDue {
			// The scan will usually take its turn before its lease expires.
			// Recheck soon, while retaining the quota's actual minimum delay.
			wait = max(wait, min(next.catalogUntil.Sub(now), time.Second))
		}
	case tinyCatalogRead:
		if !catalogDue {
			// Never sleep past the point where this live waiter gets its turn.
			priorityWait := min(next.interactiveUntil.Sub(now),
				next.catalogWaitSince.Add(tinyCatalogMaxPriorityWait).Sub(now))
			wait = max(wait, priorityWait)
		}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
		return &Reservation{RetryAfter: wait, Remaining: -1}
	}
	if wait == 0 {
		next.nextAt = now.Add(next.interval)
		if priority == tinyCatalogRead {
			// Other background waiters start a new fair turn, rather than taking
			// every slot once a single scan has aged.
			next.catalogUntil = now
		}
	}
	*b = next
	return &Reservation{Allowed: wait == 0, RetryAfter: wait, Remaining: -1}
}

type tinyReadPriorityKey struct{}
type tinyReadPriority uint8

const (
	tinyNormalRead tinyReadPriority = iota
	tinyCatalogRead
	tinyInteractiveRead
	tinyInteractiveLease        = 5 * time.Second
	tinyInteractiveMaxQuotaWait = 5 * time.Second
	// After this much priority waiting, a live background reader gets the next
	// quota-eligible slot ahead of searches. Order/comment reads remain normal.
	tinyCatalogMaxPriorityWait = 10 * time.Second
)

// WithTinyInteractiveRead lets an operator's lookup precede a catalogue scan,
// without bypassing the account quota or blocking payment/order operations.
func WithTinyInteractiveRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, tinyReadPriorityKey{}, tinyInteractiveRead)
}

// WithTinyCatalogRead marks resumable catalogue and periodic stock scans as deferrable.
func WithTinyCatalogRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, tinyReadPriorityKey{}, tinyCatalogRead)
}

func (t *requestBudget) readPriority(ctx context.Context, key string) tinyReadPriority {
	if !t.tinyPriorities || !strings.HasSuffix(key, ":read") {
		return tinyNormalRead
	}
	priority, _ := ctx.Value(tinyReadPriorityKey{}).(tinyReadPriority)
	return priority
}

func (t *Tiny) ObserveResponse(ctx context.Context, method string, status int, headers http.Header) error {
	key := t.category(method)
	remaining, remErr := strconv.Atoi(headers.Get("X-RateLimit-Remaining"))
	limit, _ := strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	reset := RetryAfter(headers, 0)
	var intervalMS int64
	// Minute quotas start at 30. A measured burst response (Limit: 4, Reset: 1)
	// describes a different window; it must not replace the minute quota.
	if limit >= 30 && limit <= 1000000 {
		intervalMS = max(1, (75000+int64(limit)-1)/int64(limit))
		if remErr == nil && remaining > 0 && reset > 0 {
			intervalMS = max(intervalMS, (reset.Milliseconds()+int64(remaining)-1)/int64(remaining))
		}
	}
	var blocked time.Duration
	if status == http.StatusTooManyRequests || (remErr == nil && remaining <= 0) {
		blocked = reset
		if blocked <= 0 {
			blocked = time.Minute
		}
	}
	if intervalMS == 0 && blocked == 0 {
		return nil
	}
	return t.update(ctx, key, intervalMS, blocked)
}

func (t *requestBudget) update(ctx context.Context, key string, intervalMS int64, blocked time.Duration) error {
	if t.pool == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		budget := t.localBudget(key)
		if intervalMS > 0 {
			budget.interval = time.Duration(intervalMS) * time.Millisecond
		}
		if until := t.now().Add(blocked); until.After(budget.blockedUntil) {
			budget.blockedUntil = until
		}
		return nil
	}
	initialInterval := intervalMS
	if initialInterval == 0 {
		initialInterval = t.defaultIntervalMS
	}
	_, err := t.pool.Exec(ctx, `INSERT INTO api_rate_budgets(account_key,interval_ms,blocked_until)
  VALUES($1,$2,clock_timestamp()+$3*interval '1 millisecond')
  ON CONFLICT(account_key) DO UPDATE SET
  interval_ms=CASE WHEN $4::bigint>0 THEN EXCLUDED.interval_ms ELSE api_rate_budgets.interval_ms END,
  blocked_until=GREATEST(api_rate_budgets.blocked_until,EXCLUDED.blocked_until)`,
		key, initialInterval, blocked.Milliseconds(), intervalMS)
	return err
}

func newRequestBudget(pool *pgxpool.Pool, intervalMS int64) *requestBudget {
	return &requestBudget{pool: pool, defaultIntervalMS: intervalMS, local: make(map[string]*tinyBudget), now: time.Now}
}
