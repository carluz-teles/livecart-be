package live

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
)

// fakeIngestRepo is an in-memory IngestRepository: it maps keyword→product for
// the lookup and records emitted envelopes, deduping by DedupKey the same way
// the real outbox unique constraint does — so the dispatch idempotency contract
// is observable without a database.
type fakeIngestRepo struct {
	products     map[string]*ProductRow // keyword (upper) → product
	lookupErr    map[string]error       // keyword (upper) → forced error
	emitted      []events.Envelope
	dedupSeen    map[string]bool
	getCallCount int

	// --- Comment ingestion core state (Bloco B4b) ---
	existingComments map[string]bool        // platform_comment_id → already stored (dedup gate)
	productsByID     map[string]*ProductRow // product id → product (fallback + single-promo resolve)
	blockedHandles   map[string]bool        // normalized handle → blocked
	storeInfo        *StoreInfo             // nil → no max-quantity gate
	cartQty          map[string]int         // "event|user|product" → qty already in cart
	waitlistExists   map[string]bool        // "event|user|product" → already queued
	nextPos          int                    // GetNextWaitlistPosition return
	decrementErr     error                  // forced DecrementProductStock failure

	createdComments    []CreateLiveCommentParams
	commentResults     []string // result labels stamped via UpdateLiveCommentResult
	createdWaitlist    []CreateWaitlistItemParams
	decremented        []stockMove
	incremented        []stockMove
	sessionCommentIncs int
	eventOrderIncs     int
	commentSeq         int
}

type stockMove struct {
	productID string
	quantity  int
}

func cartKey(eventID, userID, productID string) string {
	return eventID + "|" + userID + "|" + productID
}

func newFakeIngestRepo() *fakeIngestRepo {
	return &fakeIngestRepo{
		products:         map[string]*ProductRow{},
		lookupErr:        map[string]error{},
		dedupSeen:        map[string]bool{},
		existingComments: map[string]bool{},
		productsByID:     map[string]*ProductRow{},
		blockedHandles:   map[string]bool{},
		cartQty:          map[string]int{},
		waitlistExists:   map[string]bool{},
	}
}

func (f *fakeIngestRepo) LiveCommentExistsByPlatformID(_ context.Context, platformCommentID string) (bool, error) {
	return f.existingComments[platformCommentID], nil
}

func (f *fakeIngestRepo) CreateLiveComment(_ context.Context, params CreateLiveCommentParams) (string, error) {
	f.createdComments = append(f.createdComments, params)
	f.commentSeq++
	return fmt.Sprintf("comment-%d", f.commentSeq), nil
}

func (f *fakeIngestRepo) UpdateLiveCommentResult(_ context.Context, _ string, _ bool, _ string, _ int, result string) error {
	f.commentResults = append(f.commentResults, result)
	return nil
}

func (f *fakeIngestRepo) GetProductByID(_ context.Context, _, productID string) (*ProductRow, error) {
	return f.productsByID[productID], nil
}

func (f *fakeIngestRepo) IncrementLiveSessionComments(_ context.Context, _ string) error {
	f.sessionCommentIncs++
	return nil
}

func (f *fakeIngestRepo) IncrementLiveEventOrders(_ context.Context, _ string) error {
	f.eventOrderIncs++
	return nil
}

func (f *fakeIngestRepo) IsHandleBlocked(_ context.Context, _, handle string) (bool, error) {
	return f.blockedHandles[handle], nil
}

func (f *fakeIngestRepo) GetStoreInfo(_ context.Context, _ string) (*StoreInfo, error) {
	return f.storeInfo, nil
}

func (f *fakeIngestRepo) GetProductQuantityInUserCart(_ context.Context, eventID, userID, productID string) (int, error) {
	return f.cartQty[cartKey(eventID, userID, productID)], nil
}

func (f *fakeIngestRepo) DecrementProductStock(_ context.Context, productID string, quantity int) error {
	if f.decrementErr != nil {
		return f.decrementErr
	}
	f.decremented = append(f.decremented, stockMove{productID, quantity})
	return nil
}

func (f *fakeIngestRepo) IncrementProductStock(_ context.Context, productID string, quantity int) error {
	f.incremented = append(f.incremented, stockMove{productID, quantity})
	return nil
}

func (f *fakeIngestRepo) GetWaitlistItemByEventUserProduct(_ context.Context, eventID, userID, productID string) (bool, error) {
	return f.waitlistExists[cartKey(eventID, userID, productID)], nil
}

func (f *fakeIngestRepo) GetNextWaitlistPosition(_ context.Context, _, _ string) (int, error) {
	return f.nextPos, nil
}

func (f *fakeIngestRepo) CreateWaitlistItem(_ context.Context, params CreateWaitlistItemParams) (string, error) {
	f.createdWaitlist = append(f.createdWaitlist, params)
	f.commentSeq++
	return fmt.Sprintf("waitlist-%d", f.commentSeq), nil
}

func (f *fakeIngestRepo) GetProductByKeyword(_ context.Context, _, keyword string) (*ProductRow, error) {
	f.getCallCount++
	if err := f.lookupErr[keyword]; err != nil {
		return nil, err
	}
	return f.products[keyword], nil
}

func (f *fakeIngestRepo) EmitCommentReceived(_ context.Context, env events.Envelope) error {
	if env.DedupKey != "" && f.dedupSeen[env.DedupKey] {
		return nil // already emitted — outbox dedup no-op
	}
	if env.DedupKey != "" {
		f.dedupSeen[env.DedupKey] = true
	}
	f.emitted = append(f.emitted, env)
	return nil
}

func newTestService(repo IngestRepository) *Service {
	s := &Service{logger: zap.NewNop()}
	s.SetIngestRepository(repo)
	return s
}

func TestService_DispatchCommentReceived(t *testing.T) {
	ctx := context.Background()

	t.Run("emits a canonical comment.received envelope", func(t *testing.T) {
		repo := newFakeIngestRepo()
		s := newTestService(repo)

		input := map[string]string{"CommentID": "c1", "Text": "quero 1001"}
		if err := s.DispatchCommentReceived(ctx, "c1", "m1", events.SourceInstagramLive, input); err != nil {
			t.Fatalf("DispatchCommentReceived() error = %v", err)
		}

		if len(repo.emitted) != 1 {
			t.Fatalf("emitted count = %d, want 1", len(repo.emitted))
		}
		env := repo.emitted[0]
		if env.Name != events.CommentReceived {
			t.Errorf("Name = %q, want %q", env.Name, events.CommentReceived)
		}
		if env.Source != events.SourceInstagramLive {
			t.Errorf("Source = %q, want %q", env.Source, events.SourceInstagramLive)
		}
		if env.DedupKey != "comment.received:c1" {
			t.Errorf("DedupKey = %q, want %q", env.DedupKey, "comment.received:c1")
		}
		if env.Metadata["comment_id"] != "c1" || env.Metadata["media_id"] != "m1" {
			t.Errorf("Metadata = %v, want comment_id=c1 media_id=m1", env.Metadata)
		}
		// Payload must be the caller's input marshaled verbatim (wire contract).
		want, _ := json.Marshal(input)
		if string(env.Payload) != string(want) {
			t.Errorf("Payload = %s, want %s", env.Payload, want)
		}
	})

	t.Run("idempotent: same comment_id dispatched twice emits once", func(t *testing.T) {
		repo := newFakeIngestRepo()
		s := newTestService(repo)

		input := map[string]string{"CommentID": "dup"}
		for i := 0; i < 2; i++ {
			if err := s.DispatchCommentReceived(ctx, "dup", "m1", events.SourceInstagramLive, input); err != nil {
				t.Fatalf("dispatch #%d error = %v", i, err)
			}
		}

		if len(repo.emitted) != 1 {
			t.Fatalf("emitted count = %d, want 1 (deduped by DedupKey)", len(repo.emitted))
		}
	})

	t.Run("errors when the ingest port is not wired", func(t *testing.T) {
		s := &Service{logger: zap.NewNop()}
		err := s.DispatchCommentReceived(ctx, "c1", "m1", events.SourceInstagramLive, map[string]string{})
		if err == nil {
			t.Fatal("DispatchCommentReceived() error = nil, want non-nil when port unwired")
		}
	})
}

func TestService_FindProductByKeyword(t *testing.T) {
	ctx := context.Background()
	product := &ProductRow{ID: "p1", Keyword: "1001", Name: "Boné"}

	t.Run("match: returns the product for a present keyword", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["1001"] = product
		s := newTestService(repo)

		got := s.FindProductByKeyword(ctx, "store1", "quero 1001")
		if got == nil || got.ID != "p1" {
			t.Fatalf("FindProductByKeyword() = %+v, want product p1", got)
		}
	})

	t.Run("no-match: returns nil when no keyword maps to a product", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["9999"] = product // different keyword
		s := newTestService(repo)

		if got := s.FindProductByKeyword(ctx, "store1", "quero 1001"); got != nil {
			t.Fatalf("FindProductByKeyword() = %+v, want nil", got)
		}
	})

	t.Run("no-keyword: returns nil without touching the repo", func(t *testing.T) {
		repo := newFakeIngestRepo()
		s := newTestService(repo)

		if got := s.FindProductByKeyword(ctx, "store1", "quero 2 unidades"); got != nil {
			t.Fatalf("FindProductByKeyword() = %+v, want nil", got)
		}
		if repo.getCallCount != 0 {
			t.Errorf("GetProductByKeyword called %d times, want 0 (no keyword in text)", repo.getCallCount)
		}
	})

	t.Run("lookup error on one keyword is skipped so a later keyword still matches", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.lookupErr["X1Y2"] = context.DeadlineExceeded
		repo.products["Z3W4"] = product
		s := newTestService(repo)

		got := s.FindProductByKeyword(ctx, "store1", "quero X1Y2 e Z3W4")
		if got == nil || got.ID != "p1" {
			t.Fatalf("FindProductByKeyword() = %+v, want product p1 after skipping errored keyword", got)
		}
	})

	t.Run("returns nil when the ingest port is not wired", func(t *testing.T) {
		s := &Service{logger: zap.NewNop()}
		if got := s.FindProductByKeyword(ctx, "store1", "quero 1001"); got != nil {
			t.Fatalf("FindProductByKeyword() = %+v, want nil when port unwired", got)
		}
	})
}
