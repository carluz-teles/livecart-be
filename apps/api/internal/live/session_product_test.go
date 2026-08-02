package live

// N2 — a semântica de "lista vazia" era OPOSTA nas duas metades do sistema:
// no checkout liberava tudo, na ingestão de post/story bloqueava tudo. Estes
// testes fixam a regra única: LISTA VAZIA = TODOS OS PRODUTOS LIBERADOS.
//
// Cobre também a regra de UNIÃO do checkout, que existe porque o carrinho é do
// EVENTO e atravessa N sessões — não há "a sessão do checkout".

import (
	"context"
	"fmt"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

type whitelistFixture struct {
	storeID  string
	eventID  string
	sessions []string
	products []string
}

func seedWhitelistFixture(t *testing.T, ctx context.Context, slug string, sessionCount int) whitelistFixture {
	t.Helper()
	f := whitelistFixture{}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1,$1) RETURNING id::text`, slug,
	).Scan(&f.storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1,'active',$2, now() + interval '7 days') RETURNING id::text`,
		f.storeID, slug,
	).Scan(&f.eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	for i := 1; i <= sessionCount; i++ {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO live_sessions (event_id, status, type, sequence_order) VALUES ($1,'active','live',$2) RETURNING id::text`,
			f.eventID, i,
		).Scan(&id); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		f.sessions = append(f.sessions, id)
	}
	for i, kw := range []string{"AAA1", "BBB1"} {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
			 VALUES ($1,$2,$3,'manual',1000,10,true) RETURNING id::text`,
			f.storeID, fmt.Sprintf("Produto %d", i), kw,
		).Scan(&id); err != nil {
			t.Fatalf("seed product: %v", err)
		}
		f.products = append(f.products, id)
	}
	return f
}

func (f whitelistFixture) allowed(t *testing.T, ctx context.Context, productID string) bool {
	t.Helper()
	row, err := testQueries.GetEventProductConfigFromSessions(ctx, sqlc.GetEventProductConfigFromSessionsParams{
		EventID: mustUUID(t, f.eventID),
		ID:      mustUUID(t, productID),
		StoreID: mustUUID(t, f.storeID),
	})
	if err != nil {
		t.Fatalf("GetEventProductConfigFromSessions: %v", err)
	}
	return row.IsAllowed.Bool
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	uid, err := parseUUID(s)
	if err != nil {
		t.Fatalf("uuid %q: %v", s, err)
	}
	return uid
}

// Lista vazia libera tudo — no checkout E na ingestão, que é a regra que a N2
// unificou.
func TestSessionWhitelistEmptyAllowsEverything(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-empty", 1)

	products, err := testRepo.ListSessionProducts(ctx, f.sessions[0])
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(products) != 0 {
		t.Fatalf("sessao nova devia nascer VAZIA, veio com %d produto(s)", len(products))
	}
	for _, p := range f.products {
		if !f.allowed(t, ctx, p) {
			t.Errorf("lista vazia devia liberar o produto %s", p)
		}
	}
}

// Com whitelist, quem não está nela é barrado.
func TestSessionWhitelistBlocksProductOutsideTheList(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-block", 1)

	if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
		SessionID: f.sessions[0],
		ProductID: f.products[0],
	}); err != nil {
		t.Fatalf("UpsertSessionProduct: %v", err)
	}

	if !f.allowed(t, ctx, f.products[0]) {
		t.Error("produto NA whitelist devia ser aceito")
	}
	if f.allowed(t, ctx, f.products[1]) {
		t.Error("produto FORA da whitelist devia ser barrado")
	}
}

// A regra de união do checkout: o carrinho é do evento, então basta UMA sessão
// aceitar. Uma sessão sem lista aceita tudo — logo, ela sozinha derruba a
// barreira das outras. É consequência direta de "vazia = libera tudo", e está
// aqui explícito para ninguém descobrir isso em produção.
func TestEventWhitelistIsTheUnionOfItsSessions(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-uniao", 2)

	// Só a primeira sessão tem barreira; a segunda continua vazia.
	if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
		SessionID: f.sessions[0],
		ProductID: f.products[0],
	}); err != nil {
		t.Fatalf("UpsertSessionProduct: %v", err)
	}
	if !f.allowed(t, ctx, f.products[1]) {
		t.Error("uma sessao sem whitelist libera tudo para o carrinho do evento")
	}

	// Com barreira nas DUAS, o produto de fora passa a ser recusado.
	if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
		SessionID: f.sessions[1],
		ProductID: f.products[0],
	}); err != nil {
		t.Fatalf("UpsertSessionProduct: %v", err)
	}
	if f.allowed(t, ctx, f.products[1]) {
		t.Error("com whitelist em TODAS as sessoes, o produto de fora tem de ser barrado")
	}
}

// A rota legada por evento escreve em todas as sessões existentes — é o que
// mantém o frontend atual funcionando durante o expand.
func TestEventLevelWriteReachesEverySession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-legado", 2)

	if err := testRepo.UpsertProductInAllEventSessions(ctx, SessionProductInput{
		EventID:   f.eventID,
		ProductID: f.products[0],
	}); err != nil {
		t.Fatalf("UpsertProductInAllEventSessions: %v", err)
	}
	for i, sessionID := range f.sessions {
		products, err := testRepo.ListSessionProducts(ctx, sessionID)
		if err != nil {
			t.Fatalf("ListSessionProducts: %v", err)
		}
		if len(products) != 1 {
			t.Errorf("sessao %d ficou com %d produto(s), quero 1", i, len(products))
		}
	}

	// A leitura por evento devolve UMA linha por produto, não uma por sessão.
	union, err := testRepo.ListEventWhitelist(ctx, f.eventID)
	if err != nil {
		t.Fatalf("ListEventWhitelist: %v", err)
	}
	if len(union) != 1 {
		t.Errorf("uniao devolveu %d linhas, quero 1 (uma por PRODUTO)", len(union))
	}

	if err := testRepo.DeleteProductFromAllEventSessions(ctx, f.eventID, f.products[0]); err != nil {
		t.Fatalf("DeleteProductFromAllEventSessions: %v", err)
	}
	count, err := testRepo.CountEventWhitelist(ctx, f.eventID)
	if err != nil {
		t.Fatalf("CountEventWhitelist: %v", err)
	}
	if count != 0 {
		t.Errorf("delete por evento deixou %d produto(s) para tras", count)
	}
}

func TestSessionProductRequestValidate(t *testing.T) {
	validID := "11111111-1111-4111-8111-111111111111"
	price := int64(-1)
	qty := int32(0)

	tests := []struct {
		name  string
		req   SessionProductRequest
		field string
	}{
		{"valido", SessionProductRequest{ProductID: validID}, ""},
		{"sem produto", SessionProductRequest{}, "productId"},
		{"produto nao-uuid", SessionProductRequest{ProductID: "nao-e-uuid"}, "productId"},
		{"preco negativo", SessionProductRequest{ProductID: validID, SpecialPrice: &price}, "specialPrice"},
		{"quantidade zero", SessionProductRequest{ProductID: validID, MaxQuantity: &qty}, "maxQuantity"},
		{"ordem negativa", SessionProductRequest{ProductID: validID, DisplayOrder: -1}, "displayOrder"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("devia passar: %v", err)
				}
				return
			}
			errs, ok := err.(validation.Errors)
			if !ok {
				t.Fatalf("esperava validation.Errors, veio %T (%v)", err, err)
			}
			if _, found := errs[tc.field]; !found {
				t.Errorf("esperava erro no campo %q, veio %v", tc.field, errs)
			}
		})
	}
}

// A SESSÃO NOVA HERDA A WHITELIST DO EVENTO (decisão do dono).
//
// Sem herança a barreira era inútil no fluxo real: o lojista configura os
// produtos do evento, cria a transmissão de terça e a sessão nova nasce VAZIA —
// e "lista vazia = libera tudo" (regra que continua valendo) faz essa sessão
// sozinha liberar o catálogo inteiro, porque a união do checkout aceita o
// produto se ALGUMA sessão o aceita. É exatamente o cenário que
// TestEventWhitelistIsTheUnionOfItsSessions já documentava como consequência
// esperada — e que, no caminho de criar sessão, virava um buraco.
func TestNewSessionInheritsEventWhitelist(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-heranca", 1)

	price := int64(700)
	qty := int32(3)
	if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
		SessionID:    f.sessions[0],
		ProductID:    f.products[0],
		SpecialPrice: &price,
		MaxQuantity:  &qty,
		DisplayOrder: 2,
		Featured:     true,
	}); err != nil {
		t.Fatalf("UpsertSessionProduct: %v", err)
	}
	if f.allowed(t, ctx, f.products[1]) {
		t.Fatal("pré-condição quebrada: com uma sessão só e whitelist, o produto de fora tem de estar barrado")
	}

	// A transmissão seguinte, criada pelo caminho real do painel.
	session, _, inherited, err := testRepo.CreateSessionWithPlatformTx(ctx, f.eventID, "live", "instagram", "media-heranca")
	if err != nil {
		t.Fatalf("CreateSessionWithPlatformTx: %v", err)
	}
	if inherited != 1 {
		t.Errorf("herdou %d produto(s), quero 1", inherited)
	}

	products, err := testRepo.ListSessionProducts(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(products) != 1 || products[0].ProductID != f.products[0] {
		t.Fatalf("sessão nova nasceu com %d produto(s) — vazia, ela libera o catálogo inteiro", len(products))
	}
	// A configuração viaja junto: herdar só o id deixaria o preço especial e o
	// teto para trás e o comprador pagaria outro valor na transmissão nova.
	if products[0].SpecialPrice == nil || *products[0].SpecialPrice != price {
		t.Errorf("special_price herdado = %v, quero %d", products[0].SpecialPrice, price)
	}
	if products[0].MaxQuantity == nil || *products[0].MaxQuantity != qty {
		t.Errorf("max_quantity herdado = %v, quero %d", products[0].MaxQuantity, qty)
	}

	// O que importa de verdade: a barreira do evento continua de pé.
	if !f.allowed(t, ctx, f.products[0]) {
		t.Error("produto da whitelist deixou de ser aceito depois da nova sessão")
	}
	if f.allowed(t, ctx, f.products[1]) {
		t.Error("a sessão nova derrubou a barreira do evento — é o bug que a herança fecha")
	}
}

// Evento SEM whitelist continua liberando tudo: a herança copia uma lista
// vazia, não inventa barreira nenhuma.
func TestNewSessionInheritsNothingFromEmptyEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "wl-heranca-vazia", 1)

	session, _, inherited, err := testRepo.CreateSessionWithPlatformTx(ctx, f.eventID, "live", "instagram", "media-vazia")
	if err != nil {
		t.Fatalf("CreateSessionWithPlatformTx: %v", err)
	}
	if inherited != 0 {
		t.Errorf("herdou %d produto(s) de um evento sem whitelist", inherited)
	}
	products, err := testRepo.ListSessionProducts(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(products) != 0 {
		t.Errorf("sessão nasceu com %d produto(s) num evento sem whitelist", len(products))
	}
	for _, p := range f.products {
		if !f.allowed(t, ctx, p) {
			t.Errorf("evento sem whitelist tem de liberar o produto %s", p)
		}
	}
}
