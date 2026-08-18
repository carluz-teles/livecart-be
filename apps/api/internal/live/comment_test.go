package live

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// fakeCommentCore stubs the commentCore seam so the ingestion core can be
// exercised without a database: it returns scripted session/event/product list
// and records AddToCart calls (and can force errors).
//
// A lista de produtos é scriptada POR SESSÃO (whitelistBySession), porque é essa
// a unidade da regra: transmissões diferentes da MESMA campanha têm listas
// diferentes, e o fake precisa ser capaz de mostrar isso.
type fakeCommentCore struct {
	session      *SessionOutput
	sessionErr   error
	sessionCalls int

	event    *EventOutput
	eventErr error

	whitelistBySession map[string][]SessionProductOutput
	whitelistErr       error

	addResult AddToCartOutput
	addErr    error
	// addErrOnCall faz o N-ésimo AddToCart falhar (1-indexado). Serve ao caso de
	// um comentário com vários produtos em que só um item quebra.
	addErrOnCall int
	addCalls     []AddToCartInput
}

// scriptWhitelist é o açúcar dos testes de sessão única: grava a lista da sessão
// que o fake devolve.
func (f *fakeCommentCore) scriptWhitelist(products ...SessionProductOutput) *fakeCommentCore {
	if f.whitelistBySession == nil {
		f.whitelistBySession = map[string][]SessionProductOutput{}
	}
	f.whitelistBySession[f.session.ID] = products
	return f
}

func (f *fakeCommentCore) GetSessionByPlatformLiveID(_ context.Context, _ string) (*SessionOutput, error) {
	f.sessionCalls++
	return f.session, f.sessionErr
}

func (f *fakeCommentCore) GetEventByPlatformLiveID(_ context.Context, _ string) (*EventOutput, error) {
	return f.event, f.eventErr
}

func (f *fakeCommentCore) AddToCart(_ context.Context, input AddToCartInput) (AddToCartOutput, error) {
	f.addCalls = append(f.addCalls, input)
	if f.addErrOnCall > 0 && len(f.addCalls) == f.addErrOnCall {
		return AddToCartOutput{}, errors.New("add falhou neste item")
	}
	return f.addResult, f.addErr
}

func (f *fakeCommentCore) ListSessionWhitelist(_ context.Context, sessionID string) ([]SessionProductOutput, error) {
	return f.whitelistBySession[sessionID], f.whitelistErr
}

// fakeStockReserver records the two StockReserver surfaces.
type fakeStockReserver struct {
	noted    []ReserveParams
	erpCalls []stockMove
	noteErr  error
	erpErr   error
}

func (f *fakeStockReserver) NoteReserved(_ context.Context, p ReserveParams) error {
	f.noted = append(f.noted, p)
	return f.noteErr
}

func (f *fakeStockReserver) ReserveStockInERP(_ context.Context, _, _, _, productID string, quantity int, _ int64, _ string) error {
	f.erpCalls = append(f.erpCalls, stockMove{productID, quantity})
	return f.erpErr
}

// fakeSocialReplier records DMs and comment replies.
type fakeSocialReplier struct {
	dms     []socialMsg
	replies []socialMsg
}

type socialMsg struct {
	target string // recipient IGSID (DM) or comment id (reply)
	text   string
}

func (f *fakeSocialReplier) SendInstagramDM(_ context.Context, _, recipientID, text string) error {
	f.dms = append(f.dms, socialMsg{recipientID, text})
	return nil
}

func (f *fakeSocialReplier) ReplyToInstagramComment(_ context.Context, _, commentID, text string) error {
	f.replies = append(f.replies, socialMsg{commentID, text})
	return nil
}

// fakeBillingGate returns a fixed blocked verdict.
type fakeBillingGate struct{ blocked bool }

func (f fakeBillingGate) IsStoreBlocked(_ context.Context, _ string) bool { return f.blocked }

// fakeWebhookAuditor records audited webhook events.
type fakeWebhookAuditor struct{ stored []StoreWebhookInput }

func (f *fakeWebhookAuditor) StoreWebhookEvent(_ context.Context, input StoreWebhookInput) error {
	f.stored = append(f.stored, input)
	return nil
}

// newCommentTestService builds a Service wired with the ingest repo and the core
// seam, ready for per-test port injection.
func newCommentTestService(repo IngestRepository, core commentCore) *Service {
	s := &Service{logger: zap.NewNop(), core: core}
	s.SetIngestRepository(repo)
	return s
}

// liveEvent returns an active live event (post-commerce rules off).
func liveEvent() *EventOutput {
	return &EventOutput{ID: "ev1", StoreID: "store1", Title: "Live", Status: "active"}
}

func TestService_ProcessInstagramComment_Dedup(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	repo.existingComments["c1"] = true // already stored
	core := &fakeCommentCore{}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Text: "quero 1001"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if core.sessionCalls != 0 {
		t.Errorf("session lookups = %d, want 0 (dedup must short-circuit before any effect)", core.sessionCalls)
	}
	if len(core.addCalls) != 0 {
		t.Errorf("AddToCart calls = %d, want 0 on a duplicate comment", len(core.addCalls))
	}
	if len(repo.createdComments) != 0 {
		t.Errorf("comments created = %d, want 0 on a duplicate comment", len(repo.createdComments))
	}
}

func TestService_ProcessInstagramComment_KeywordMatch(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Price: 1000, Stock: 10, Name: "Boné"}
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true, TotalItems: 1, TotalCents: 1000},
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 1001"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if len(core.addCalls) != 1 {
		t.Fatalf("AddToCart calls = %d, want 1", len(core.addCalls))
	}
	if got := core.addCalls[0]; got.ProductID != "p1" || got.Quantity != 1 || got.WaitlistedQuantity != 0 {
		t.Errorf("AddToCart input = %+v, want product p1 qty 1 waitlisted 0", got)
	}
	if len(stock.noted) != 1 || stock.noted[0].Op != stockOpCartAdd || stock.noted[0].CartID != "cart1" || stock.noted[0].Quantity != 1 {
		t.Errorf("NoteReserved = %+v, want one cart_add of qty 1 keyed to cart1", stock.noted)
	}
	if len(stock.erpCalls) != 1 || stock.erpCalls[0].quantity != 1 {
		t.Errorf("ReserveStockInERP = %+v, want one call of qty 1", stock.erpCalls)
	}
	if len(repo.commentResults) != 1 || repo.commentResults[0] != "added_to_cart" {
		t.Errorf("comment results = %v, want [added_to_cart]", repo.commentResults)
	}
}

func TestService_ProcessInstagramComment_NoIntent(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	core := &fakeCommentCore{session: &SessionOutput{ID: "sess1"}, event: liveEvent()}
	s := newCommentTestService(repo, core)

	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Text: "produto muito lindo"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if len(core.addCalls) != 0 {
		t.Errorf("AddToCart calls = %d, want 0 for a non-intent comment", len(core.addCalls))
	}
	if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "no_intent" {
		t.Fatalf("created comments = %+v, want one with result no_intent", repo.createdComments)
	}
}

func TestService_ProcessInstagramComment_BillingBlocked(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	core := &fakeCommentCore{session: &SessionOutput{ID: "sess1"}, event: liveEvent()}
	s := newCommentTestService(repo, core)
	s.billingGate = fakeBillingGate{blocked: true}

	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Text: "quero 1001"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if len(core.addCalls) != 0 {
		t.Errorf("AddToCart calls = %d, want 0 when the store is blocked", len(core.addCalls))
	}
	if len(repo.createdComments) != 0 {
		t.Errorf("comments created = %d, want 0 (billing gate returns before persisting)", len(repo.createdComments))
	}
}

func TestService_ProcessInstagramComment_PartialFulfillment(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	repo.nextPos = 3
	repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Price: 500, Stock: 2, Name: "Boné"}
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	// stock 2, requested 5 → 2 available + 3 waitlisted
	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 5 1001"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if len(core.addCalls) != 1 || core.addCalls[0].Quantity != 5 || core.addCalls[0].WaitlistedQuantity != 3 {
		t.Fatalf("AddToCart input = %+v, want qty 5 waitlisted 3", core.addCalls)
	}
	if len(stock.noted) != 1 || stock.noted[0].Quantity != 2 {
		t.Errorf("NoteReserved = %+v, want one reservation of the 2 available units", stock.noted)
	}
	if len(repo.createdWaitlist) != 1 || repo.createdWaitlist[0].Quantity != 3 || repo.createdWaitlist[0].CartID != "cart1" || repo.createdWaitlist[0].Position != 3 {
		t.Errorf("waitlist rows = %+v, want one of qty 3 on cart1 at position 3", repo.createdWaitlist)
	}
	if len(repo.commentResults) != 1 || repo.commentResults[0] != "partial_fulfillment" {
		t.Errorf("comment results = %v, want [partial_fulfillment]", repo.commentResults)
	}
}

func TestService_ProcessInstagramComment_OutOfStockWaitlistOnly(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Price: 500, Stock: 0, Name: "Boné"}
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 1001"}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}

	if len(stock.noted) != 0 {
		t.Errorf("NoteReserved calls = %d, want 0 when nothing is available", len(stock.noted))
	}
	if len(repo.decremented) != 0 {
		t.Errorf("stock decrements = %d, want 0 when nothing is available", len(repo.decremented))
	}
	if len(repo.createdWaitlist) != 1 || repo.createdWaitlist[0].Quantity != 1 {
		t.Errorf("waitlist rows = %+v, want one of qty 1", repo.createdWaitlist)
	}
	if len(repo.commentResults) != 1 || repo.commentResults[0] != "waitlisted" {
		t.Errorf("comment results = %v, want [waitlisted]", repo.commentResults)
	}
}

func TestService_ProcessInstagramComment_RollbackOnAddToCartFailure(t *testing.T) {
	ctx := context.Background()
	repo := newFakeIngestRepo()
	repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Price: 500, Stock: 10, Name: "Boné"}
	core := &fakeCommentCore{
		session: &SessionOutput{ID: "sess1"},
		event:   liveEvent(),
		addErr:  errors.New("db down"),
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 1001"})
	if err == nil {
		t.Fatal("ProcessInstagramComment() error = nil, want AddToCart failure to propagate")
	}
	if len(repo.decremented) != 1 || len(repo.incremented) != 1 || repo.incremented[0].quantity != 1 {
		t.Errorf("stock moves = dec %+v inc %+v, want the reserved unit rolled back", repo.decremented, repo.incremented)
	}
	if len(stock.noted) != 0 {
		t.Errorf("NoteReserved calls = %d, want 0 when AddToCart failed", len(stock.noted))
	}
}

func TestService_ProcessInstagramComment_PostRules(t *testing.T) {
	ctx := context.Background()

	postEvent := func() *EventOutput {
		return &EventOutput{ID: "ev1", StoreID: "store1", Title: "Post", Status: "active"}
	}

	t.Run("not in promo: replies and does not add", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Stock: 5, Name: "Boné"}
		core := (&fakeCommentCore{
			session: &SessionOutput{ID: "sess1", Type: "post"},
			event:   postEvent(),
		}).scriptWhitelist(SessionProductOutput{ProductID: "other", ProductActive: true, Stock: 5, Keyword: "2002", Name: "Caneca"})
		s := newCommentTestService(repo, core)
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Username: "ana", Text: "quero 1001"}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(social.replies) != 1 {
			t.Errorf("comment replies = %d, want 1 (not-in-promo)", len(social.replies))
		}
		if len(core.addCalls) != 0 {
			t.Errorf("AddToCart calls = %d, want 0", len(core.addCalls))
		}
		if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "not_in_promo" {
			t.Errorf("created comments = %+v, want result not_in_promo", repo.createdComments)
		}
	})

	t.Run("single-promo bare trigger auto-adds", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.productsByID["p1"] = &ProductRow{ID: "p1", Keyword: "1001", Stock: 5, Price: 300, Name: "Boné"}
		core := (&fakeCommentCore{
			session:   &SessionOutput{ID: "sess1", Type: "post"},
			event:     postEvent(),
			addResult: AddToCartOutput{CartID: "cart1", IsNewCart: true},
		}).scriptWhitelist(SessionProductOutput{ProductID: "p1", ProductActive: true, Stock: 5, Keyword: "1001", Name: "Boné"})
		s := newCommentTestService(repo, core)
		s.stockReserver = &fakeStockReserver{}
		s.socialReplier = &fakeSocialReplier{}

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Username: "ana", Text: "EU QUERO"}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(core.addCalls) != 1 || core.addCalls[0].ProductID != "p1" {
			t.Fatalf("AddToCart calls = %+v, want one for the single promo product", core.addCalls)
		}
	})

	t.Run("multi-promo bare trigger asks for a keyword", func(t *testing.T) {
		repo := newFakeIngestRepo()
		core := (&fakeCommentCore{
			session: &SessionOutput{ID: "sess1", Type: "post"},
			event:   postEvent(),
		}).scriptWhitelist(
			SessionProductOutput{ProductID: "p1", ProductActive: true, Stock: 5, Keyword: "1001", Name: "Boné"},
			SessionProductOutput{ProductID: "p2", ProductActive: true, Stock: 5, Keyword: "2002", Name: "Caneca"},
		)
		s := newCommentTestService(repo, core)
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Username: "ana", Text: "EU QUERO"}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(social.replies) != 1 {
			t.Errorf("comment replies = %d, want 1 (needs_keyword)", len(social.replies))
		}
		if len(core.addCalls) != 0 {
			t.Errorf("AddToCart calls = %d, want 0", len(core.addCalls))
		}
		if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "needs_keyword" {
			t.Errorf("created comments = %+v, want result needs_keyword", repo.createdComments)
		}
	})

	t.Run("in-promo but out of stock replies", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Stock: 0, Name: "Boné"}
		core := (&fakeCommentCore{
			session: &SessionOutput{ID: "sess1", Type: "post"},
			event:   postEvent(),
		}).scriptWhitelist(SessionProductOutput{ProductID: "p1", ProductActive: true, Stock: 0, Keyword: "1001", Name: "Boné"})
		s := newCommentTestService(repo, core)
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", Username: "ana", Text: "quero 1001"}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(social.replies) != 1 {
			t.Errorf("comment replies = %d, want 1 (out_of_stock)", len(social.replies))
		}
		if len(core.addCalls) != 0 {
			t.Errorf("AddToCart calls = %d, want 0", len(core.addCalls))
		}
		if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "out_of_stock" {
			t.Errorf("created comments = %+v, want result out_of_stock", repo.createdComments)
		}
	})

	t.Run("story channel replies by DM, not comment", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Stock: 5, Name: "Boné"}
		core := (&fakeCommentCore{
			session: &SessionOutput{ID: "sess1", Type: "story"},
			event:   &EventOutput{ID: "ev1", StoreID: "store1", Status: "active"},
		}).scriptWhitelist(SessionProductOutput{ProductID: "other", ProductActive: true, Stock: 5, Keyword: "2002", Name: "Caneca"})
		s := newCommentTestService(repo, core)
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{CommentID: "c1", MediaID: "m1", UserID: "igsid1", Username: "ana", Channel: "dm", Text: "quero 1001"}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(social.dms) != 1 || social.dms[0].target != "igsid1" {
			t.Errorf("DMs = %+v, want one to igsid1", social.dms)
		}
		if len(social.replies) != 0 {
			t.Errorf("comment replies = %d, want 0 on the DM channel", len(social.replies))
		}
	})
}

// A REGRA DESTA RODADA, no cenário que o dono do produto descreveu:
//
//	"tem live que o cliente quer vender qualquer coisa... e Post que o cliente
//	 quer vender apenas o produto X e story que o cliente quer vender apenas
//	 produto Y."
//
// As três transmissões são da MESMA campanha e estão no ar AO MESMO TEMPO. É
// por isso que a lista não pode ser do evento: uma lista só não consegue dizer
// "tudo", "só X" e "só Y" ao mesmo tempo.
//
// Cada linha é um comentário caindo numa das três transmissões; o veredito
// depende exclusivamente da lista DAQUELA sessão.
func TestSameEventDifferentSessionsDifferentProductLists(t *testing.T) {
	ctx := context.Background()

	const (
		sessionLive  = "sess-live"
		sessionPost  = "sess-post"
		sessionStory = "sess-story"
	)

	produtoX := SessionProductOutput{ProductID: "pX", ProductActive: true, Stock: 5, Keyword: "1001", Name: "Vestido"}
	produtoY := SessionProductOutput{ProductID: "pY", ProductActive: true, Stock: 5, Keyword: "2002", Name: "Bolsa"}

	// A campanha inteira: uma live sem lista (vende tudo), um post restrito ao
	// produto X e um story restrito ao produto Y.
	listasDaCampanha := map[string][]SessionProductOutput{
		sessionLive:  nil,
		sessionPost:  {produtoX},
		sessionStory: {produtoY},
	}

	tests := []struct {
		name        string
		sessionID   string
		sessionType string
		text        string
		wantProduct string // "" = nada entrou no carrinho
		wantResult  string
		wantReplies int
	}{
		{"live vende o produto X", sessionLive, "live", "quero 1001", "pX", "added_to_cart", 0},
		{"live vende tambem o produto Y", sessionLive, "live", "quero 2002", "pY", "added_to_cart", 0},
		{"post vende o produto X", sessionPost, "post", "quero 1001", "pX", "added_to_cart", 0},
		{"post RECUSA o produto Y", sessionPost, "post", "quero 2002", "", "not_in_promo", 1},
		{"story vende o produto Y", sessionStory, "story", "quero 2002", "pY", "added_to_cart", 0},
		{"story RECUSA o produto X", sessionStory, "story", "quero 1001", "", "not_in_promo", 1},
	}

	// O rótulo do comentário sai por dois caminhos: quem entra no carrinho é
	// carimbado depois (commentResults), quem é recusado nasce já classificado
	// (createdComments). commentResult junta os dois para a tabela poder falar
	// só de veredito.
	commentResult := func(repo *fakeIngestRepo) []string {
		if len(repo.commentResults) > 0 {
			return repo.commentResults
		}
		out := make([]string, len(repo.createdComments))
		for i, c := range repo.createdComments {
			out[i] = c.Result
		}
		return out
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeIngestRepo()
			repo.products["1001"] = &ProductRow{ID: "pX", Keyword: "1001", Price: 1000, Stock: 5, Name: "Vestido"}
			repo.products["2002"] = &ProductRow{ID: "pY", Keyword: "2002", Price: 2000, Stock: 5, Name: "Bolsa"}

			core := &fakeCommentCore{
				// Mesma campanha nas três linhas: o que muda é só a transmissão.
				event:              &EventOutput{ID: "ev-guarda-chuva", StoreID: "store1", Status: "active"},
				session:            &SessionOutput{ID: tc.sessionID, Type: tc.sessionType},
				whitelistBySession: listasDaCampanha,
				addResult:          AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
			}
			s := newCommentTestService(repo, core)
			s.stockReserver = &fakeStockReserver{}
			social := &fakeSocialReplier{}
			s.socialReplier = social

			if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
				CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: tc.text,
			}); err != nil {
				t.Fatalf("ProcessInstagramComment() error = %v", err)
			}

			if tc.wantProduct == "" {
				if len(core.addCalls) != 0 {
					t.Errorf("AddToCart = %+v, quero nenhum: a transmissao nao vende esse produto", core.addCalls)
				}
			} else {
				if len(core.addCalls) != 1 || core.addCalls[0].ProductID != tc.wantProduct {
					t.Fatalf("AddToCart = %+v, quero um para %s", core.addCalls, tc.wantProduct)
				}
			}
			if got := commentResult(repo); len(got) != 1 || got[0] != tc.wantResult {
				t.Errorf("resultado do comentario = %v, quero [%s]", got, tc.wantResult)
			}
			if len(social.replies) != tc.wantReplies {
				t.Errorf("respostas ao comprador = %d, quero %d", len(social.replies), tc.wantReplies)
			}
		})
	}
}

// Post/story com lista VAZIA vende tudo — a mesma regra do checkout e da live.
//
// É o caso que a remoção da herança torna comum: toda transmissão criada pelo
// painel nasce vazia. Antes desta rodada, a ingestão de post/story fazia o
// OPOSTO do checkout e recusava 100% dos comentários numa lista vazia.
func TestPostSessionWithEmptyListSellsAnything(t *testing.T) {
	ctx := context.Background()

	t.Run("codigo conhecido entra no carrinho", func(t *testing.T) {
		repo := newFakeIngestRepo()
		repo.products["1001"] = &ProductRow{ID: "p1", Keyword: "1001", Price: 1000, Stock: 5, Name: "Boné"}
		core := (&fakeCommentCore{
			session:   &SessionOutput{ID: "sess1", Type: "post"},
			event:     &EventOutput{ID: "ev1", StoreID: "store1", Status: "active"},
			addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
		}).scriptWhitelist() // nasce vazia
		s := newCommentTestService(repo, core)
		s.stockReserver = &fakeStockReserver{}
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
			CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 1001",
		}); err != nil {
			t.Fatalf("ProcessInstagramComment() error = %v", err)
		}

		if len(core.addCalls) != 1 || core.addCalls[0].ProductID != "p1" {
			t.Fatalf("AddToCart = %+v, quero um para p1: lista vazia libera todo o catalogo", core.addCalls)
		}
		if len(social.replies) != 0 {
			t.Errorf("respostas = %+v, quero nenhuma: nao ha o que recusar numa lista vazia", social.replies)
		}
	})

	t.Run("EU QUERO pelado fica sem produto, e em silencio", func(t *testing.T) {
		repo := newFakeIngestRepo()
		core := (&fakeCommentCore{
			session: &SessionOutput{ID: "sess1", Type: "post"},
			event:   &EventOutput{ID: "ev1", StoreID: "store1", Status: "active"},
		}).scriptWhitelist()
		s := newCommentTestService(repo, core)
		social := &fakeSocialReplier{}
		s.socialReplier = social

		if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
			CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "EU QUERO",
		}); err != nil {
			t.Fatalf("ProcessInstagramComment() error = %v", err)
		}

		if len(core.addCalls) != 0 {
			t.Errorf("AddToCart = %+v, quero nenhum: nao da para adivinhar o produto de uma loja inteira", core.addCalls)
		}
		if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "no_product" {
			t.Errorf("resultado = %+v, quero [no_product]", repo.createdComments)
		}
		if len(social.replies) != 0 || len(social.dms) != 0 {
			t.Errorf("respostas = %+v / DMs = %+v, quero nenhuma: 'nao disponivel nesta promocao' seria mentira", social.replies, social.dms)
		}
	})
}

// Um REEL respeita a lista da transmissão, igual a um post e a um story.
//
// Reel entra no sistema por DOIS caminhos que EXIGEM pelo menos um produto
// (publicar pelo LiveCart, integration.publishInstagramReelEvent, e mapear uma
// publicação existente com type='reel'), e desde a 000122 a sessão diz mesmo
// 'reel' em vez de se achatar em 'post'. Se a ingestão não reconhecer o tipo, a
// barreira que o lojista acabou de configurar no formulário some em silêncio: o
// reel passa a vender o catálogo inteiro.
//
// É exatamente o modo de falha que IsPostCommerceSessionType documenta
// ("esquecer 'reel' aqui faria todo Reel perder as regras em silêncio").
func TestReelHonoursItsProductList(t *testing.T) {
	ctx := context.Background()

	repo := newFakeIngestRepo()
	repo.products["1001"] = &ProductRow{ID: "pX", Keyword: "1001", Price: 1000, Stock: 5, Name: "Vestido"}
	repo.products["2002"] = &ProductRow{ID: "pY", Keyword: "2002", Price: 2000, Stock: 5, Name: "Bolsa"}

	core := (&fakeCommentCore{
		session:   &SessionOutput{ID: "sess-reel", Type: SessionTypeReel},
		event:     &EventOutput{ID: "ev1", StoreID: "store1", Status: "active"},
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}).scriptWhitelist(SessionProductOutput{
		ProductID: "pX", ProductActive: true, Stock: 5, Keyword: "1001", Name: "Vestido",
	})
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}
	social := &fakeSocialReplier{}
	s.socialReplier = social

	// O produto de FORA da lista tem de ser recusado, com resposta ao comprador.
	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 2002",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}
	if len(core.addCalls) != 0 {
		t.Errorf("AddToCart = %+v, quero nenhum: 2002 nao esta na lista deste reel", core.addCalls)
	}
	if len(repo.createdComments) != 1 || repo.createdComments[0].Result != "not_in_promo" {
		t.Errorf("resultado = %+v, quero [not_in_promo]", repo.createdComments)
	}
	if len(social.replies) != 1 {
		t.Errorf("respostas = %d, quero 1: o comprador precisa saber que o produto nao esta na promocao", len(social.replies))
	}

	// E o produto DE DENTRO da lista continua entrando.
	if err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
		CommentID: "c2", MediaID: "m1", UserID: "u1", Username: "ana", Text: "quero 1001",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment() error = %v", err)
	}
	if len(core.addCalls) != 1 || core.addCalls[0].ProductID != "pX" {
		t.Fatalf("AddToCart = %+v, quero um para pX", core.addCalls)
	}
}
