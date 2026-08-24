package erp

// Import da Tiny traz TODAS as imagens do produto — o lojista escolhe a
// principal no front. Antes o parser pegava só o primeiro anexo, então quando
// a imagem "certa" não era a primeira, o lojista importava a errada sem opção.
//
// GetProduct agora devolve ImageURLs (todos os anexos com url, na ordem) e
// mantém ImageURL como default (a primeira). Anexo sem url é ignorado.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetProductTrazTodasAsImagens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 111, "descricao": "Caneca", "tipo": "S", "situacao": "A",
			"precos": {"preco": 19.90},
			"estoque": {"quantidade": 5},
			"anexos": [
				{"url": "https://tiny/img1.jpg", "externo": true},
				{"url": "", "externo": false},
				{"url": "https://tiny/img2.jpg", "externo": true},
				{"url": "https://tiny/img3.jpg", "externo": true}
			]
		}`))
	}))
	defer srv.Close()

	prod, err := newTinyAgainst(t, srv).GetProduct(context.Background(), "111")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	want := []string{"https://tiny/img1.jpg", "https://tiny/img2.jpg", "https://tiny/img3.jpg"}
	if len(prod.ImageURLs) != len(want) {
		t.Fatalf("ImageURLs = %v, quero %v (o anexo sem url deve ser ignorado)", prod.ImageURLs, want)
	}
	for i, u := range want {
		if prod.ImageURLs[i] != u {
			t.Errorf("ImageURLs[%d] = %q, quero %q (ordem do Tiny)", i, prod.ImageURLs[i], u)
		}
	}
	if prod.ImageURL != want[0] {
		t.Errorf("ImageURL default = %q, quero a primeira %q", prod.ImageURL, want[0])
	}
}

// Sem anexos: ImageURLs vazio e ImageURL vazio — o front cai no placeholder.
func TestGetProductSemImagens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":222,"descricao":"Sem foto","tipo":"S","situacao":"A","precos":{"preco":10.0},"estoque":{"quantidade":1}}`))
	}))
	defer srv.Close()

	prod, err := newTinyAgainst(t, srv).GetProduct(context.Background(), "222")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if len(prod.ImageURLs) != 0 || prod.ImageURL != "" {
		t.Errorf("sem anexos devia dar ImageURLs vazio e ImageURL vazio; veio %v / %q", prod.ImageURLs, prod.ImageURL)
	}
}
