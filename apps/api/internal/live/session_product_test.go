package live

// A lista de produtos vendáveis é da TRANSMISSÃO, e lista vazia libera TODOS os
// produtos ativos da loja. Estes testes fixam a regra no banco.
//
// Cobrem também a regra de UNIÃO do CHECKOUT, que continua sendo do evento
// porque o carrinho é do evento e atravessa N transmissões — não existe "a
// sessão do checkout".

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

// O CHECKOUT aceita o que QUALQUER transmissão da campanha aceita: o carrinho é
// do evento e atravessa todas elas, então basta UMA sessão aceitar. Uma sessão
// sem lista aceita tudo — logo, ela sozinha derruba a barreira das outras.
//
// O nome fala do CHECKOUT de propósito: isto NÃO é "a lista da campanha" (que
// não existe mais), é a regra de quem valida o carrinho. Numa campanha com uma
// live (lista vazia) e um post (lista de 1), a live libera o catálogo inteiro no
// checkout; a barreira que vale de verdade para o post é a da INGESTÃO, que lê a
// lista da sessão do comentário. Está aqui explícito para ninguém descobrir isso
// em produção.
func TestCheckoutAcceptsWhatAnySessionAccepts(t *testing.T) {
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

// A HERANÇA MORREU: a transmissão nova nasce VAZIA mesmo quando as outras da
// mesma campanha têm lista.
//
// A herança (cf2f45b) existia para um problema que só existe quando há lista de
// CAMPANHA: "configurei os produtos do evento, criei a transmissão depois, e a
// sessão vazia derrubou a barreira". Sem lista de campanha não há de onde
// herdar, e cada transmissão é configurada explicitamente — que é exatamente o
// que o dono quer, porque a live vende tudo enquanto o story vende uma peça só.
func TestNewSessionIsBornEmpty(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "sem-heranca", 1)

	price := int64(700)
	if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
		SessionID:    f.sessions[0],
		ProductID:    f.products[0],
		SpecialPrice: &price,
	}); err != nil {
		t.Fatalf("UpsertSessionProduct: %v", err)
	}

	// A transmissão seguinte, criada pelo caminho real do painel.
	session, _, err := testRepo.CreateSessionWithPlatformTx(ctx, f.eventID, "live", "instagram", "media-sem-heranca")
	if err != nil {
		t.Fatalf("CreateSessionWithPlatformTx: %v", err)
	}

	products, err := testRepo.ListSessionProducts(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(products) != 0 {
		t.Fatalf("a transmissao nova herdou %d produto(s) — a heranca devia ter saido", len(products))
	}

	// A lista da transmissão ANTERIOR não foi tocada: cada uma é a sua.
	anterior, err := testRepo.ListSessionProducts(ctx, f.sessions[0])
	if err != nil {
		t.Fatalf("ListSessionProducts (anterior): %v", err)
	}
	if len(anterior) != 1 {
		t.Errorf("a transmissao anterior ficou com %d produto(s), quero 1", len(anterior))
	}
}

// A contagem por transmissão é o que permite a tela distinguir "vende tudo" de
// "esqueci de configurar": zero é resposta legítima, não ausência de dado.
func TestCountSessionProductsByEventIsPerSession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "contagem-por-sessao", 2)

	for _, p := range f.products {
		if _, err := testRepo.UpsertSessionProduct(ctx, SessionProductInput{
			SessionID: f.sessions[0],
			ProductID: p,
		}); err != nil {
			t.Fatalf("UpsertSessionProduct: %v", err)
		}
	}

	counts, err := testRepo.CountSessionProductsByEvent(ctx, f.eventID)
	if err != nil {
		t.Fatalf("CountSessionProductsByEvent: %v", err)
	}
	if counts[f.sessions[0]] != 2 {
		t.Errorf("sessao configurada conta %d, quero 2", counts[f.sessions[0]])
	}
	if got, ok := counts[f.sessions[1]]; ok || got != 0 {
		t.Errorf("sessao sem lista conta %d (presente=%v), quero 0/ausente — zero significa 'vende tudo'", got, ok)
	}
}
