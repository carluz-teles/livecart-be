package live

// A CAMPANHA NASCE VAZIA — sem transmissão nenhuma até o lojista criar uma.
//
// Este teste já cravou o OPOSTO ("todo evento nasce com pelo menos uma sessão"),
// e a inversão é deliberada. Aquela regra existia por uma razão que morreu: a
// lista de produtos (000112) e o modo live (000113) desceram para live_sessions,
// e a rota de lista por EVENTO (POST /lives/:id/whitelist) precisava de uma
// sessão onde gravar — sem ela, escrevia zero linhas e lia 404. Essa rota não
// existe mais: a lista pertence à transmissão e só se configura lá.
//
// O que a sessão automática causava, já sem servir para nada: o painel anunciava
// "Esta campanha tem uma transmissão (Live 1)" para uma campanha recém-criada,
// vazia. Uma transmissão que o lojista nunca criou, aparecendo como se estivesse
// no ar.
//
// A sessão continua nascendo junto quando a criação TRAZ transmissão — mídia
// vinculada ou lista de produtos, que é o atalho de post/story.

import (
	"context"
	"testing"
	"time"
)

func TestCampaignStartsEmptyAndGainsSessionsOnDemand(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "sessao-sempre")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)

	t.Run("sem midia nasce sem transmissao", func(t *testing.T) {
		out, err := svc.Create(ctx, CreateLiveInput{
			StoreID: storeID,
			Title:   "Campanha sem id de live",
			Type:    "multi",
			EndsAt:  &endsAt,
		})
		if err != nil {
			t.Fatalf("criar evento sem midia: %v", err)
		}
		assertSessionCount(t, ctx, out.ID, 0)
	})

	t.Run("com midia nasce com a transmissao", func(t *testing.T) {
		platform, mediaID := "instagram", "media-sessao-sempre"
		out, err := svc.Create(ctx, CreateLiveInput{
			StoreID:        storeID,
			Title:          "Campanha com id de live",
			Type:           "multi",
			Platform:       &platform,
			PlatformLiveID: &mediaID,
			EndsAt:         &endsAt,
		})
		if err != nil {
			t.Fatalf("criar evento com midia: %v", err)
		}
		assertSessionCount(t, ctx, out.ID, 1)

		var bound int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM live_session_platforms lsp
			   JOIN live_sessions ls ON ls.id = lsp.session_id
			  WHERE ls.event_id = $1::uuid`, out.ID,
		).Scan(&bound); err != nil {
			t.Fatalf("contar midias: %v", err)
		}
		if bound != 1 {
			t.Errorf("evento criado com mídia deveria ter 1 vínculo, tem %d", bound)
		}
	})
}

// O caminho completo do painel, agora que a campanha nasce vazia: marcar a
// campanha, criar a transmissão depois e só então escolher os produtos dela.
//
// Este é o fluxo REAL — o lojista marca a "Semana Black" na segunda e pendura a
// live quando ela existe. Antes o teste se apoiava na sessão que vinha de graça
// com o evento; apoiar-se nela escondia justamente o passo que o lojista faz.
func TestSessionProductsWorkOnSessionCreatedAfterTheCampaign(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "produtos-sem-midia")
	svc := newWindowService(nil)

	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Vestido','VEST','manual',9900,5,true) RETURNING id::text`, storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed produto: %v", err)
	}

	endsAt := time.Now().Add(24 * time.Hour)
	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID,
		Title:   "Campanha nova",
		Type:    "multi",
		EndsAt:  &endsAt,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}
	assertSessionCount(t, ctx, out.ID, 0)

	// A transmissão entra depois, sem mídia — o lojista ainda não tem o id da
	// live. É o caminho que o formulário "Nova sessão" usa.
	created, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: out.ID,
		StoreID: storeID,
		Type:    "live",
	})
	if err != nil {
		t.Fatalf("criar transmissao depois da campanha: %v", err)
	}
	sessionID := created.ID
	assertSessionCount(t, ctx, out.ID, 1)

	added, err := svc.UpsertSessionProduct(ctx, SessionProductInput{
		StoreID:   storeID,
		EventID:   out.ID,
		SessionID: sessionID,
		ProductID: productID,
	})
	if err != nil {
		t.Fatalf("adicionar produto na transmissao: %v", err)
	}
	if added.ProductID != productID {
		t.Errorf("produto devolvido %q, esperado %q", added.ProductID, productID)
	}

	list, err := svc.ListSessionProducts(ctx, sessionID, out.ID, storeID)
	if err != nil {
		t.Fatalf("listar produtos da transmissao: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("a transmissao deveria ter 1 produto, tem %d", len(list))
	}
}

// O atalho de post grava os produtos na SESSÃO que nasce com ele, DENTRO da
// transação de criação.
//
// Antes era um laço posterior chamando a rota por EVENTO (broadcast em todas as
// sessões), que só acertava por acidente — naquele instante existia uma sessão
// só — e cujo erro por produto era apenas um Warn: a publicação já estava no ar
// e a campanha ficava com lista PARCIAL, ou vazia, que sob "vazia vende tudo"
// libera o catálogo inteiro. Exatamente o oposto do que o lojista escolheu.
func TestCreatePostEventWritesProductsIntoItsOwnSession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "post-produtos-na-sessao")
	svc := newWindowService(nil)

	productIDs := make([]string, 0, 2)
	for _, kw := range []string{"PST1", "PST2"} {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
			 VALUES ($1,$2,$3,'manual',5000,5,true) RETURNING id::text`, storeID, "Produto "+kw, kw,
		).Scan(&id); err != nil {
			t.Fatalf("seed produto: %v", err)
		}
		productIDs = append(productIDs, id)
	}

	endsAt := time.Now().Add(24 * time.Hour)
	out, err := svc.CreatePostEvent(ctx, CreatePostInput{
		StoreID:    storeID,
		Type:       "post",
		Title:      "Promo do post",
		MediaID:    "media-post-produtos",
		ProductIDs: productIDs,
		EndsAt:     &endsAt,
	})
	if err != nil {
		t.Fatalf("CreatePostEvent: %v", err)
	}

	sessionID := firstSessionID(t, ctx, out.ID)
	list, err := testRepo.ListSessionProducts(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(list) != len(productIDs) {
		t.Fatalf("a transmissao do post ficou com %d produto(s), quero %d", len(list), len(productIDs))
	}
	for i, p := range list {
		if p.DisplayOrder != int32(i) {
			t.Errorf("produto %d veio com display_order %d, quero %d (a ordem escolhida no formulario)", i, p.DisplayOrder, i)
		}
	}
}

// Um produto inexistente derruba a criação INTEIRA: não existe evento com lista
// parcial. Lista parcial é afrouxamento silencioso da barreira.
func TestCreatePostEventIsAtomicOnBadProduct(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "post-produto-invalido")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(24 * time.Hour)
	_, err := svc.CreatePostEvent(ctx, CreatePostInput{
		StoreID:    storeID,
		Type:       "post",
		Title:      "Promo quebrada",
		MediaID:    "media-post-invalido",
		ProductIDs: []string{"11111111-1111-4111-8111-111111111111"},
		EndsAt:     &endsAt,
	})
	if err == nil {
		t.Fatal("CreatePostEvent devia falhar com produto inexistente, em vez de publicar com lista vazia")
	}

	var events int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM live_events WHERE store_id = $1::uuid`, storeID,
	).Scan(&events); err != nil {
		t.Fatalf("contar eventos: %v", err)
	}
	if events != 0 {
		t.Errorf("sobraram %d evento(s) apos a falha — a criacao tem de ser atomica", events)
	}
}

// firstSessionID devolve a sessão que nasceu com o evento.
func firstSessionID(t *testing.T, ctx context.Context, eventID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM live_sessions WHERE event_id = $1::uuid ORDER BY sequence_order ASC LIMIT 1`, eventID,
	).Scan(&id); err != nil {
		t.Fatalf("buscar sessao do evento %s: %v", eventID, err)
	}
	return id
}

func assertSessionCount(t *testing.T, ctx context.Context, eventID string, want int) {
	t.Helper()
	var got int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM live_sessions WHERE event_id = $1::uuid`, eventID,
	).Scan(&got); err != nil {
		t.Fatalf("contar sessoes: %v", err)
	}
	if got != want {
		t.Errorf("evento %s tem %d sessao(oes), esperado %d", eventID, got, want)
	}
}
