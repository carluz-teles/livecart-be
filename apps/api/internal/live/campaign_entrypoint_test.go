package live

// A PORTA DE ENTRADA da campanha guarda-chuva.
//
// O modelo do épico diz: o EVENTO é a campanha (dono do carrinho, do prazo e
// das regras) e a SESSÃO é cada transmissão (dona do tipo e da mídia). O banco
// já era assim desde a 000122, que dropou live_events.type — mas a criação
// continuava traduzindo uma escolha feita ANTES de a campanha existir, e o que
// ela conseguia produzir era sempre uma campanha de uma sessão só.
//
// Estes testes prendem os três contratos que faltavam para o painel conseguir
// oferecer "crio a campanha agora, penduro as transmissões depois":
//
//  1. o tipo pedido na criação é o tipo da PRIMEIRA SESSÃO, no vocabulário real
//     (live|post|reel|story) e não só no legado (single|multi);
//  2. dá para acrescentar transmissão SEM mídia e numa campanha AGENDADA — que
//     é o caso de uso central ("marco a Semana Black hoje, escolho a live de
//     segunda depois"), e era exatamente o que estava fechado;
//  3. a publicação vinculada por sessão fica com os MESMOS metadados que ela
//     teria se tivesse entrado como evento novo.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
)

// sessionTypesOf devolve os tipos das sessões do evento, em ordem de sequência.
func sessionTypesOf(t *testing.T, ctx context.Context, eventID string) []string {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT type FROM live_sessions WHERE event_id = $1::uuid ORDER BY sequence_order`, eventID)
	if err != nil {
		t.Fatalf("ler tipos das sessoes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan tipo: %v", err)
		}
		out = append(out, typ)
	}
	return out
}

// TestCreateEventTypesTheFirstSessionNotTheEvent prova que `type` na criação
// atravessa até live_sessions.type — e que o vocabulário legado continua
// aceito, caindo em 'live' como sempre caiu.
//
// Antes, 'single' e 'multi' eram os ÚNICOS valores que a validação aceitava, e
// os dois caem no mesmo default. Ou seja: o único controle de tipo da porta de
// entrada não mudava um byte no banco, enquanto a espécie de verdade era
// decidida por qual formulário o lojista tinha aberto.
func TestCreateEventTypesTheFirstSessionNotTheEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "primeira-sessao-tipada")
	svc := newWindowService(nil)
	endsAt := time.Now().Add(48 * time.Hour)

	casos := []struct {
		pedido string
		quer   string
	}{
		{"", SessionTypeLive},         // omitido: o default histórico
		{"single", SessionTypeLive},   // legado: "uma live"
		{"multi", SessionTypeLive},    // legado: "várias plataformas"
		{"live", SessionTypeLive},     // vocabulário real
		{"post", SessionTypePost},     //
		{"reel", SessionTypeReel},     //
		{"story", SessionTypeStory},   //
		{"qualquer", SessionTypeLive}, // desconhecido não pode virar 500 no CHECK
	}
	// A criação vem COM mídia porque é isso que faz nascer a transmissão: uma
	// campanha vazia não tem sessão nenhuma, e não haveria tipo para conferir.
	// O que está sob teste é o mapeamento do vocabulário pedido para o tipo da
	// SESSÃO — não a existência dela.
	platform := "instagram"
	for i, caso := range casos {
		mediaID := fmt.Sprintf("media-tipo-%d", i)
		out, err := svc.Create(ctx, CreateLiveInput{
			StoreID:        storeID,
			Title:          fmt.Sprintf("Campanha %d", i),
			Type:           caso.pedido,
			Platform:       &platform,
			PlatformLiveID: &mediaID,
			EndsAt:         &endsAt,
		})
		if err != nil {
			t.Fatalf("criar evento com type=%q: %v", caso.pedido, err)
		}
		got := sessionTypesOf(t, ctx, out.ID)
		if len(got) != 1 || got[0] != caso.quer {
			t.Errorf("type=%q gerou sessoes %v, quero [%s]", caso.pedido, got, caso.quer)
		}
	}
}

// TestCreateSessionWithoutMediaOnScheduledEvent é o caso de uso do épico que
// estava fechado por dois lados ao mesmo tempo: platformLiveId era `required` no
// DTO e o repositório sempre gravava a plataforma.
//
// A campanha agendada tem de aceitar transmissão, e a transmissão tem de poder
// nascer sem publicação — o lojista marca a Semana Black hoje e só na segunda
// sabe o id da live.
func TestCreateSessionWithoutMediaOnScheduledEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "sessao-sem-midia-agendada")
	svc := newWindowService(nil)

	scheduledAt := time.Now().Add(24 * time.Hour)
	endsAt := scheduledAt.Add(72 * time.Hour)
	campanha, err := svc.Create(ctx, CreateLiveInput{
		StoreID:     storeID,
		Title:       "Semana Black",
		ScheduledAt: &scheduledAt,
		EndsAt:      &endsAt,
	})
	if err != nil {
		t.Fatalf("criar campanha agendada: %v", err)
	}
	if campanha.Status != "scheduled" {
		t.Fatalf("campanha nasceu %q, o teste precisa dela agendada", campanha.Status)
	}

	sessao, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: campanha.ID,
		StoreID: storeID,
		Type:    SessionTypeLive,
	})
	if err != nil {
		t.Fatalf("acrescentar transmissao sem midia em campanha agendada: %v", err)
	}
	if sessao.Platform != nil {
		t.Errorf("sessao sem midia devolveu plataforma %+v — a tela pintaria um vinculo que nao existe", sessao.Platform)
	}

	var vinculos int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM live_session_platforms WHERE session_id = $1::uuid`, sessao.ID,
	).Scan(&vinculos); err != nil {
		t.Fatalf("contar vinculos: %v", err)
	}
	if vinculos != 0 {
		t.Errorf("sessao sem midia gravou %d vinculo(s) em live_session_platforms", vinculos)
	}

	// A campanha nasceu VAZIA e ganhou exatamente esta transmissão — não há mais
	// a sessão de brinde que o evento criava sozinho.
	if got := sessionTypesOf(t, ctx, campanha.ID); len(got) != 1 {
		t.Errorf("campanha tem %d transmissoes (%v), quero 1", len(got), got)
	}
}

// TestCreateSessionRejectsHalfBoundMedia — meia mídia grava uma linha que não
// resolve comentário nenhum e ainda mente na tela dizendo que a transmissão
// está vinculada. Ou vêm os dois campos, ou nenhum.
func TestCreateSessionRejectsHalfBoundMedia(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "sessao-meia-midia")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)
	campanha, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Semana Black", EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar campanha: %v", err)
	}

	if _, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: campanha.ID, StoreID: storeID, Type: SessionTypePost,
		Platform: "instagram", // sem o id da mídia
	}); err == nil {
		t.Error("plataforma sem platformLiveId foi aceita")
	}
	if _, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: campanha.ID, StoreID: storeID, Type: SessionTypePost,
		PlatformLiveID: "media-orfa", // sem a plataforma
	}); err == nil {
		t.Error("platformLiveId sem plataforma foi aceito")
	}
}

// seedStoreProduct cria um produto direto na loja (o seedProduct do pacote
// resolve a loja a partir de um evento, e aqui o evento ainda não existe).
func seedStoreProduct(t *testing.T, ctx context.Context, storeID string, price int64) string {
	t.Helper()
	n := time.Now().UnixNano()
	var id string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Vestido','none',$2,$3,$4,100) RETURNING id::text`,
		storeID, fmt.Sprintf("ext-%d", n), fmt.Sprintf("%04d", n%10000), price,
	).Scan(&id); err != nil {
		t.Fatalf("seed produto: %v", err)
	}
	return id
}

// TestCreatePostEventKeepsTheMediaKind — o Reel escolhido no grid do painel tem
// de nascer 'reel'.
//
// O grid mostra "Reel" no card, mas a rota de evento-de-post ignorava a espécie
// e gravava sempre 'post'. O MESMO Reel publicado PELO LiveCart já nascia
// 'reel' (publishInstagramReelEvent manda SessionTypeReel), então duas mídias
// idênticas ficavam com tipos diferentes conforme a porta por onde entraram — e
// o rótulo errado vazava para a métrica por transmissão e para a DM do
// comprador, que chama a sessão de "Post 2" em vez de "Reel 2".
func TestCreatePostEventKeepsTheMediaKind(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "post-event-especie-da-midia")
	svc := newWindowService(nil)
	produto := seedStoreProduct(t, ctx, storeID, 2500)
	endsAt := time.Now().Add(48 * time.Hour)

	casos := []struct {
		pedido string
		quer   string
	}{
		{"", SessionTypePost}, // omitido: o default histórico desta rota
		{SessionTypePost, SessionTypePost},
		{SessionTypeReel, SessionTypeReel},
		{SessionTypeStory, SessionTypeStory},
	}
	for i, caso := range casos {
		out, err := svc.CreatePostEvent(ctx, CreatePostInput{
			StoreID:    storeID,
			Type:       caso.pedido,
			Title:      fmt.Sprintf("Promo %d", i),
			MediaID:    fmt.Sprintf("media-especie-%d-%d", i, time.Now().UnixNano()),
			ProductIDs: []string{produto},
			EndsAt:     &endsAt,
		})
		if err != nil {
			t.Fatalf("criar evento de publicacao com type=%q: %v", caso.pedido, err)
		}
		got := sessionTypesOf(t, ctx, out.ID)
		if len(got) != 1 || got[0] != caso.quer {
			t.Errorf("type=%q gerou sessoes %v, quero [%s]", caso.pedido, got, caso.quer)
		}
	}
}

// TestCreateSessionStoresMediaMetadata — a MESMA publicação tem de ficar igual
// venha de onde vier. Como evento novo ela passava por CreatePostEvent, que
// chama SetMedia; como sessão de um evento existente ela não passava por lugar
// nenhum e ficava sem permalink, sem capa e sem legenda.
func TestCreateSessionStoresMediaMetadata(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "sessao-metadados-midia")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)
	campanha, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Semana Black", EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar campanha: %v", err)
	}

	const mediaID = "media-com-metadados"
	sessao, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID:           campanha.ID,
		StoreID:           storeID,
		Type:              SessionTypeReel,
		Platform:          "instagram",
		PlatformLiveID:    mediaID,
		MediaPermalink:    "https://instagram.com/reel/abc",
		MediaThumbnailURL: "https://cdn.example/capa.jpg",
		MediaCaption:      "Reel da quarta",
	})
	if err != nil {
		t.Fatalf("criar sessao com midia: %v", err)
	}
	if sessao.Type != SessionTypeReel {
		t.Errorf("sessao nasceu %q, quero reel", sessao.Type)
	}

	var permalink, thumb, caption *string
	if err := testPool.QueryRow(ctx,
		`SELECT media_permalink, media_thumbnail_url, media_caption
		   FROM live_session_platforms WHERE platform_live_id = $1`, mediaID,
	).Scan(&permalink, &thumb, &caption); err != nil {
		t.Fatalf("ler metadados da midia: %v", err)
	}
	if permalink == nil || *permalink != "https://instagram.com/reel/abc" {
		t.Errorf("permalink = %v, quero o do payload", permalink)
	}
	if thumb == nil || *thumb != "https://cdn.example/capa.jpg" {
		t.Errorf("thumbnail = %v, quero a do payload", thumb)
	}
	if caption == nil || *caption != "Reel da quarta" {
		t.Errorf("legenda = %v, quero a do payload", caption)
	}
}

// A VALIDAÇÃO do DTO é a camada que o painel bate de verdade — o service pode
// aceitar 'story' e a requisição morrer em 422 antes de chegar nele. Este teste
// roda o mesmo validator.New() do main.go sobre os dois payloads de criação.
//
// Sem ele, a única prova do vocabulário novo seria o teste de service, que
// pula o handler inteiro.
func TestCreationRequestsAcceptTheSessionVocabulary(t *testing.T) {
	v := validator.New()
	endsAt := "2026-12-01T20:00:00Z"

	t.Run("CreateLiveRequest", func(t *testing.T) {
		aceitos := []string{"", "single", "multi", "live", "post", "reel", "story"}
		for _, typ := range aceitos {
			req := CreateLiveRequest{Title: "Semana Black", Type: typ, EndsAt: &endsAt}
			if err := v.Struct(req); err != nil {
				t.Errorf("type=%q recusado: %v", typ, err)
			}
		}
		// 'nenhum' não é vocabulário de sessão: a validação tem de barrar antes
		// de o default silencioso transformá-lo em live.
		req := CreateLiveRequest{Title: "Semana Black", Type: "nenhum", EndsAt: &endsAt}
		if err := v.Struct(req); err == nil {
			t.Error("type=nenhum passou na validacao")
		}
	})

	t.Run("CreatePostRequest", func(t *testing.T) {
		aceitos := []string{"", "post", "reel", "story"}
		for _, typ := range aceitos {
			req := CreatePostRequest{
				Type: typ, MediaID: "media-1", ProductIDs: []string{"p1"}, EndsAt: &endsAt,
			}
			if err := v.Struct(req); err != nil {
				t.Errorf("type=%q recusado: %v", typ, err)
			}
		}
		// 'live' não: esta rota mapeia publicação já existente, e publicação não
		// é transmissão ao vivo.
		req := CreatePostRequest{
			Type: "live", MediaID: "media-1", ProductIDs: []string{"p1"}, EndsAt: &endsAt,
		}
		if err := v.Struct(req); err == nil {
			t.Error("type=live passou na validacao do evento de publicacao")
		}
	})

	t.Run("CreateSessionRequest sem midia", func(t *testing.T) {
		// O caso "decidir depois": nem plataforma, nem id.
		if err := v.Struct(CreateSessionRequest{Type: "live"}); err != nil {
			t.Errorf("sessao sem midia recusada na validacao: %v", err)
		}
	})
}

// TestLinkSessionMediaClosesTheLaterPromise — "crie a transmissão agora e
// vincule a publicação depois" só é verdade se o depois existir.
//
// Criar sessão sem mídia já funcionava, e o painel passou a anunciar isso com o
// badge "Sem publicação vinculada" e a ajuda "vincule quando a publicação
// existir". Só que o único vínculo posterior era POST /lives/:id/platforms, que
// escolhe a sessão sozinha (a mais recente no ar): numa campanha com duas
// transmissões ele grava na errada e ninguém percebe, porque o comentário
// simplesmente continua não virando carrinho.
//
// Este teste prende as duas metades: o vínculo cai na sessão NOMEADA, com os
// metadados da publicação, e sessão de outro evento é recusada.
func TestLinkSessionMediaClosesTheLaterPromise(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "vincular-midia-depois")
	svc := newWindowService(nil)
	endsAt := time.Now().Add(48 * time.Hour)

	campanha, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Semana Black", EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar campanha: %v", err)
	}

	// Duas transmissões sem mídia: é justamente o caso em que a rota por evento
	// não tem como acertar qual delas o lojista quis vincular.
	primeira, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: campanha.ID, StoreID: storeID, Type: SessionTypeLive,
	})
	if err != nil {
		t.Fatalf("criar primeira transmissao: %v", err)
	}
	segunda, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: campanha.ID, StoreID: storeID, Type: SessionTypePost,
	})
	if err != nil {
		t.Fatalf("criar segunda transmissao: %v", err)
	}

	const mediaID = "media-vinculada-depois"
	out, err := svc.LinkSessionMedia(ctx, LinkSessionMediaInput{
		StoreID:           storeID,
		EventID:           campanha.ID,
		SessionID:         primeira.ID,
		Platform:          "instagram",
		PlatformLiveID:    mediaID,
		MediaPermalink:    "https://instagram.com/p/xyz",
		MediaThumbnailURL: "https://cdn.example/capa.jpg",
		MediaCaption:      "Post de quarta",
	})
	if err != nil {
		t.Fatalf("vincular midia na sessao: %v", err)
	}
	if out.SessionID != primeira.ID {
		t.Errorf("vinculo caiu na sessao %s, quero %s", out.SessionID, primeira.ID)
	}

	var sessaoDoVinculo string
	var permalink, thumb, caption *string
	if err := testPool.QueryRow(ctx,
		`SELECT session_id::text, media_permalink, media_thumbnail_url, media_caption
		   FROM live_session_platforms WHERE platform_live_id = $1`, mediaID,
	).Scan(&sessaoDoVinculo, &permalink, &thumb, &caption); err != nil {
		t.Fatalf("ler vinculo: %v", err)
	}
	if sessaoDoVinculo != primeira.ID {
		t.Errorf("banco gravou o vinculo na sessao %s, quero %s", sessaoDoVinculo, primeira.ID)
	}
	if permalink == nil || *permalink != "https://instagram.com/p/xyz" {
		t.Errorf("permalink = %v, quero o do payload — a publicacao vinculada depois "+
			"tem de ficar igual a que entrou na criacao", permalink)
	}
	if thumb == nil || *thumb != "https://cdn.example/capa.jpg" {
		t.Errorf("thumbnail = %v, quero a do payload", thumb)
	}
	if caption == nil || *caption != "Post de quarta" {
		t.Errorf("legenda = %v, quero a do payload", caption)
	}

	// A outra transmissão continua sem mídia — vincular uma não pode "resolver"
	// a campanha inteira.
	var vinculosDaSegunda int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM live_session_platforms WHERE session_id = $1::uuid`, segunda.ID,
	).Scan(&vinculosDaSegunda); err != nil {
		t.Fatalf("contar vinculos da segunda: %v", err)
	}
	if vinculosDaSegunda != 0 {
		t.Errorf("segunda transmissao ganhou %d vinculo(s) sem ninguem pedir", vinculosDaSegunda)
	}

	// Posse: sessão de OUTRA campanha não pode ser vinculada por este evento.
	outra, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Outra campanha", EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar outra campanha: %v", err)
	}
	sessaoAlheia, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: outra.ID, StoreID: storeID, Type: SessionTypeLive,
	})
	if err != nil {
		t.Fatalf("criar sessao da outra campanha: %v", err)
	}
	if _, err := svc.LinkSessionMedia(ctx, LinkSessionMediaInput{
		StoreID: storeID, EventID: campanha.ID, SessionID: sessaoAlheia.ID,
		Platform: "instagram", PlatformLiveID: "media-de-outro-evento",
	}); err == nil {
		t.Error("vinculo aceitou sessao de outra campanha")
	}
}

// TestLinkSessionMediaRequestRequiresTheMediaID — nesta rota a mídia é o
// assunto. Diferente de CreateSession, onde a ausência dos dois campos é um
// caminho legítimo ("decidir depois"), aqui um corpo sem platformLiveId não tem
// o que fazer e precisa morrer em 422, não gravar um vínculo vazio.
func TestLinkSessionMediaRequestRequiresTheMediaID(t *testing.T) {
	if err := (LinkSessionMediaRequest{Platform: "instagram"}).Validate(); err == nil {
		t.Error("corpo sem platformLiveId passou na validacao")
	}
	if err := (LinkSessionMediaRequest{PlatformLiveID: "media-1"}).Validate(); err != nil {
		t.Errorf("plataforma omitida recusada (%v) — o painel so tem Instagram e "+
			"o ToInput preenche o default", err)
	}
	if err := (LinkSessionMediaRequest{Platform: "tiktok", PlatformLiveID: "m"}).Validate(); err == nil {
		t.Error("plataforma nao suportada passou na validacao")
	}
	// O default do ToInput é o que evita exigir do painel um campo com um valor
	// possível só.
	if got := (LinkSessionMediaRequest{PlatformLiveID: "m"}).ToInput("s", "e", "sess").Platform; got != "instagram" {
		t.Errorf("plataforma default = %q, quero instagram", got)
	}
}

// TestCreateEventStoresFirstSessionMediaMetadata — a publicação escolhida como
// PRIMEIRA transmissão pelo formulário de campanha tem de ficar igual à que
// entra pelo caminho de evento-de-post.
//
// CreatePostEvent chamava SetMedia; Create não. Resultado: a MESMA publicação
// nascia com permalink, capa e legenda por uma porta e sem nada pela outra. A
// captura funcionava nos dois (quem resolve o comentário é o platform_live_id),
// só a tela ficava mais pobre conforme a porta — e "conforme a porta" é
// exatamente o que o épico existe para acabar.
func TestCreateEventStoresFirstSessionMediaMetadata(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "primeira-sessao-metadados")
	svc := newWindowService(nil)
	endsAt := time.Now().Add(48 * time.Hour)

	mediaID := "media-da-primeira-sessao"
	plataforma := "instagram"
	if _, err := svc.Create(ctx, CreateLiveInput{
		StoreID:           storeID,
		Title:             "Semana Black",
		Type:              SessionTypeReel,
		Platform:          &plataforma,
		PlatformLiveID:    &mediaID,
		EndsAt:            &endsAt,
		MediaPermalink:    "https://instagram.com/reel/primeira",
		MediaThumbnailURL: "https://cdn.example/primeira.jpg",
		MediaCaption:      "Reel de abertura",
	}); err != nil {
		t.Fatalf("criar campanha com publicacao na primeira transmissao: %v", err)
	}

	var permalink, thumb, caption *string
	if err := testPool.QueryRow(ctx,
		`SELECT media_permalink, media_thumbnail_url, media_caption
		   FROM live_session_platforms WHERE platform_live_id = $1`, mediaID,
	).Scan(&permalink, &thumb, &caption); err != nil {
		t.Fatalf("ler metadados da midia: %v", err)
	}
	if permalink == nil || *permalink != "https://instagram.com/reel/primeira" {
		t.Errorf("permalink = %v, quero o do payload", permalink)
	}
	if thumb == nil || *thumb != "https://cdn.example/primeira.jpg" {
		t.Errorf("thumbnail = %v, quero a do payload", thumb)
	}
	if caption == nil || *caption != "Reel de abertura" {
		t.Errorf("legenda = %v, quero a do payload", caption)
	}
}
