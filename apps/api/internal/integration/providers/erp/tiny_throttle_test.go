package erp

// Estrangulamento do Tiny não pode virar "produto não existe".
//
// O caso de campo: o lojista buscou "God Of" (o produto se chama "Jogo God Of
// War Ragnarok...") e recebeu "Produto não encontrado no ERP". O produto
// estava lá. A busca lista os produtos e depois faz UM GetProduct por
// resultado para trazer estoque e imagem — e o Tiny limita a 1 req/s. Os
// GetProduct voltavam 429, cada um virava um erro genérico, cada produto era
// descartado em silêncio, e no fim a lista vazia era reportada como ausência.
//
// Duas mentiras numa: o produto existe, e a culpa não é do lojista ter buscado
// errado. Numa outra tentativa o mesmo caminho estourou o tempo do front ("A
// busca demorou demais"), porque insistia nos 20 resultados a 1 req/s.
//
// Este teste trava a peça que sustenta a distinção: o 429 tem de chegar ao
// chamador TIPADO, para a busca poder parar e dizer a verdade.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

func newTinyAgainst(t *testing.T, srv *httptest.Server) *Tiny {
	t.Helper()
	original := tinyAPIBaseURL
	tinyAPIBaseURL = srv.URL
	t.Cleanup(func() { tinyAPIBaseURL = original })

	tiny, err := NewTiny(TinyConfig{
		IntegrationID: "int-test",
		StoreID:       "store-test",
		Credentials:   &Credentials{AccessToken: "tok"},
		Logger:        zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewTiny: %v", err)
	}
	return tiny
}

func TestGetProductTipaOEstrangulamento(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"mensagem":"too many requests"}`))
	}))
	defer srv.Close()

	tiny := newTinyAgainst(t, srv)

	_, err := tiny.GetProduct(context.Background(), "123")
	if err == nil {
		t.Fatal("GetProduct devolveu sucesso para um 429")
	}

	var rl *ratelimit.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("429 chegou como %T (%v) — a busca nao consegue distinguir "+
			"estrangulamento de 'esse produto deu problema' e acaba dizendo "+
			"ao lojista que o produto nao existe", err, err)
	}
}

// 404 continua sendo ausência de verdade: o discriminador não pode engolir o
// caso legítimo junto.
func TestGetProductMantem404ComoAusencia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tiny := newTinyAgainst(t, srv)

	_, err := tiny.GetProduct(context.Background(), "999")
	if err == nil {
		t.Fatal("GetProduct devolveu sucesso para um 404")
	}
	var rl *ratelimit.ErrRateLimited
	if errors.As(err, &rl) {
		t.Error("404 foi classificado como estrangulamento — produto realmente ausente ficaria eternamente 'tente de novo'")
	}
}

// A listagem já tipava o 429; o teste fixa isso junto para os dois caminhos
// não voltarem a divergir — foi a divergência que criou o bug.
func TestListProductsTipaOEstrangulamento(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	tiny := newTinyAgainst(t, srv)

	_, err := tiny.ListProducts(context.Background(), providers.ListProductsParams{
		Search: "God Of", PageSize: 20, ActiveOnly: true,
	})
	if err == nil {
		t.Fatal("ListProducts devolveu sucesso para um 429")
	}
	var rl *ratelimit.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("429 da listagem chegou como %T (%v)", err, err)
	}
}
