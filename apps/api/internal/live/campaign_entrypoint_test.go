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
	for i, caso := range casos {
		out, err := svc.Create(ctx, CreateLiveInput{
			StoreID: storeID,
			Title:   fmt.Sprintf("Campanha %d", i),
			Type:    caso.pedido,
			EndsAt:  &endsAt,
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

	// A campanha agora tem duas transmissões: a que nasceu com o evento e esta.
	if got := sessionTypesOf(t, ctx, campanha.ID); len(got) != 2 {
		t.Errorf("campanha tem %d transmissoes (%v), quero 2", len(got), got)
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
