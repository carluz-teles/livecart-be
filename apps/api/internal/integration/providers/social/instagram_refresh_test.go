package social

// Renovação do token de longa duração do Instagram.
//
// O caminho feliz é o menos interessante: o que precisa ficar provado são as
// DUAS recusas da Meta, porque errar qualquer uma delas faz a integração ser
// marcada como 'error' pelo chamador (Service.refreshToken) e o Instagram da
// loja para de funcionar inteiro — não só a renovação.
//
//   - token renovado há menos de 24h: a Graph recusa. Não é falha, é cedo
//     demais, e tem de terminar em "nada a fazer";
//   - token VENCIDO: não há renovação possível, só reconectar — e aí o erro é
//     o desfecho certo, porque é o estado que o lojista precisa ver.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

func newTestInstagram(t *testing.T, creds *providers.Credentials, handler http.HandlerFunc) (*Instagram, func()) {
	t.Helper()
	ig := &Instagram{
		integrationID: "int-1",
		storeID:       "store-1",
		credentials:   creds,
		logger:        zap.NewNop(),
		client:        &http.Client{Timeout: 5 * time.Second},
	}
	if handler == nil {
		return ig, func() {}
	}
	srv := httptest.NewServer(handler)
	// A base é package-level; o teste a aponta para o servidor falso e
	// restaura ao final. Por isso estes casos não usam t.Parallel().
	original := instagramGraphAPIBaseURL
	instagramGraphAPIBaseURL = srv.URL
	return ig, func() {
		instagramGraphAPIBaseURL = original
		srv.Close()
	}
}

func TestRefreshTokenSkipsWhenTooFresh(t *testing.T) {
	// Renovado há pouco: vence daqui a ~60 dias. A Graph recusaria com "token
	// must be at least 24 hours old"; chamar seria pedir para ser marcado
	// 'error' sem motivo.
	ig, done := newTestInstagram(t, &providers.Credentials{
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(60 * 24 * time.Hour),
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("nao devia chamar a Graph para um token renovado ha menos de 24h")
	})
	defer done()

	creds, err := ig.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("token novo demais nao e falha, veio: %v", err)
	}
	if creds != nil {
		t.Fatal("esperava 'nada a fazer' (nil, nil)")
	}
}

func TestRefreshTokenFailsWhenAlreadyExpired(t *testing.T) {
	ig, done := newTestInstagram(t, &providers.Credentials{
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(-time.Hour),
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("nao adianta chamar a Graph com token vencido")
	})
	defer done()

	if _, err := ig.RefreshToken(context.Background()); err == nil {
		t.Fatal("token vencido tem de virar erro — a loja precisa reconectar, e isso e visivel no status da integracao")
	}
}

func TestRefreshTokenRenewsAndKeepsExtraCredentials(t *testing.T) {
	ig, done := newTestInstagram(t, &providers.Credentials{
		AccessToken: "velho",
		ExpiresAt:   time.Now().Add(3 * 24 * time.Hour),
		Extra:       map[string]any{"instagram_user_id": "17841400000000000"},
	}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("grant_type"); got != "ig_refresh_token" {
			t.Errorf("grant_type errado: %q", got)
		}
		if got := r.URL.Query().Get("access_token"); got != "velho" {
			t.Errorf("a renovacao usa o PROPRIO token de longa duracao, veio %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"novo","token_type":"bearer","expires_in":5183944}`))
	})
	defer done()

	creds, err := ig.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("renovar: %v", err)
	}
	if creds == nil || creds.AccessToken != "novo" {
		t.Fatalf("esperava o token novo, veio %+v", creds)
	}
	if time.Until(creds.ExpiresAt) < 55*24*time.Hour {
		t.Fatalf("expires_at devia ir para ~60 dias, veio %s", creds.ExpiresAt)
	}
	// Extra carrega o instagram_user_id, que é como o webhook resolve a loja.
	// Perdê-lo na renovação quebraria a venda por DM sem nenhum erro visível.
	if creds.Extra == nil || creds.Extra["instagram_user_id"] != "17841400000000000" {
		t.Fatalf("a renovacao apagou credenciais que nao vem na resposta: %+v", creds.Extra)
	}
}

func TestRefreshTokenSurfacesGraphError(t *testing.T) {
	ig, done := newTestInstagram(t, &providers.Credentials{
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(3 * 24 * time.Hour),
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token"}}`))
	})
	defer done()

	if _, err := ig.RefreshToken(context.Background()); err == nil {
		t.Fatal("erro da Graph tem de subir — e ele que marca a integracao como 'error'")
	}
}
