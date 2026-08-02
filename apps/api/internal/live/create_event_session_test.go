package live

// D1 — todo evento nasce com pelo menos UMA sessão, mesmo sem mídia.
//
// Por que este teste existe: a whitelist (000110) e o modo live (000111)
// desceram de live_events para live_sessions. A criação de evento tinha DOIS
// caminhos, e o caminho sem platformLiveId — que é o padrão do formulário do
// painel, porque o lojista raramente já tem o id da live na hora de marcar a
// campanha — criava o evento SEM sessão nenhuma. A partir daí:
//
//   - POST /lives/:id/whitelist gravava ZERO linhas (o INSERT ... SELECT não
//     tinha sessão para casar) e a leitura de volta respondia 404 "produto nao
//     esta na whitelist do evento";
//   - destacar produto / pausar processamento virava no-op SILENCIOSO.
//
// Ou seja: configurar produto no caminho padrão de criação parava de funcionar.

import (
	"context"
	"testing"
	"time"
)

func TestCreateEventAlwaysHasASession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "sessao-sempre")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)

	t.Run("sem midia", func(t *testing.T) {
		out, err := svc.Create(ctx, CreateLiveInput{
			StoreID: storeID,
			Title:   "Campanha sem id de live",
			Type:    "multi",
			EndsAt:  &endsAt,
		})
		if err != nil {
			t.Fatalf("criar evento sem midia: %v", err)
		}
		assertSessionCount(t, ctx, out.ID, 1)
	})

	t.Run("com midia", func(t *testing.T) {
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

// TestEventWhitelistWorksOnEventCreatedWithoutMedia é o sintoma que o usuário
// via: adicionar produto pela aba "Produtos" de um evento recém-criado.
func TestEventWhitelistWorksOnEventCreatedWithoutMedia(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "whitelist-sem-midia")
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

	added, err := svc.AddEventProduct(ctx, AddEventProductInput{
		EventID:   out.ID,
		StoreID:   storeID,
		ProductID: productID,
	})
	if err != nil {
		t.Fatalf("adicionar produto na whitelist do evento: %v", err)
	}
	if added.ProductID != productID {
		t.Errorf("produto devolvido %q, esperado %q", added.ProductID, productID)
	}

	list, err := svc.ListEventProducts(ctx, out.ID, storeID)
	if err != nil {
		t.Fatalf("listar whitelist: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("whitelist deveria ter 1 produto, tem %d", len(list))
	}
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
