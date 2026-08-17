package erp

// O saldo que vale para vender é o DISPONÍVEL — e a regra estava mapeada num
// lugar só.
//
// `GET /produtos/{id}` devolve `estoque.quantidade`, o saldo FÍSICO. Quando a
// loja liga `use_available_stock`, o GetProduct consulta `/estoque/{id}` e
// troca esse número pelo disponível. Só que a troca acontecia em `out.Stock`,
// que é o estoque do produto PAI: cada variação seguia com o físico que veio
// dentro do payload do pai, onde não existe quebra de reservado.
//
// A diferença é peça reservada por orçamento salvo no Tiny. Ela continua no
// físico e sai do disponível — oferecê-la é vender o que já tem dono.
//
// Em campo: a cantodaart ligou a configuração, e produto importado continuou
// chegando com o físico.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// tinyComSaldoDisponivel monta um provider com a regra da loja LIGADA.
func tinyComSaldoDisponivel(t *testing.T, srv *httptest.Server) *Tiny {
	t.Helper()
	original := tinyAPIBaseURL
	tinyAPIBaseURL = srv.URL
	t.Cleanup(func() { tinyAPIBaseURL = original })

	tiny, err := NewTiny(TinyConfig{
		IntegrationID:     "int-test",
		StoreID:           "store-test",
		Credentials:       &Credentials{AccessToken: "tok"},
		Logger:            zap.NewNop(),
		UseAvailableStock: true,
	})
	if err != nil {
		t.Fatalf("NewTiny: %v", err)
	}
	return tiny
}

// servidorDePaiComVariacoes responde o pai com duas variações e o endpoint de
// estoque de cada id. `reservado` é o que separa físico de disponível.
func servidorDePaiComVariacoes(t *testing.T, saldos map[string][2]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "/estoque/") {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			par, conhecido := saldos[id]
			if !conhecido {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fisico, reservado := par[0], par[1]
			_, _ = fmt.Fprintf(w, `{"saldo":%d,"reservado":%d,"disponivel":%d}`,
				fisico, reservado, fisico-reservado)
			return
		}

		// GET /produtos/{id}: o payload do pai traz o FÍSICO de cada variação,
		// e não tem campo de reservado nem de disponível — é a forma real da
		// resposta do Tiny.
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fisico := saldos[id][0]
		_, _ = fmt.Fprintf(w, `{
			"id": %s,
			"nome": "Caneca Natalina",
			"tipo": "P",
			"precos": {"preco": 39.90},
			"estoque": {"quantidade": %d},
			"variacoes": [
				{"id": 900001, "descricao": "Vermelha", "estoque": {"quantidade": %d}, "precos": {"preco": 39.90}},
				{"id": 900002, "descricao": "Verde",    "estoque": {"quantidade": %d}, "precos": {"preco": 39.90}}
			]
		}`, id, fisico, saldos["900001"][0], saldos["900002"][0])
	}))
}

func TestVariacaoUsaSaldoDisponivelQuandoALojaPede(t *testing.T) {
	// pai 10 físico / 2 reservados; variações 6-1 e 4-3.
	saldos := map[string][2]int{
		"800000": {10, 2},
		"900001": {6, 1},
		"900002": {4, 3},
	}
	srv := servidorDePaiComVariacoes(t, saldos)
	defer srv.Close()

	pai, err := tinyComSaldoDisponivel(t, srv).GetProduct(context.Background(), "800000")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	if pai.Stock != 8 {
		t.Errorf("saldo do pai = %d, esperava 8 (10 físico - 2 reservados)", pai.Stock)
	}
	if len(pai.Variants) != 2 {
		t.Fatalf("esperava 2 variações, veio %d", len(pai.Variants))
	}

	querido := map[string]int{"900001": 5, "900002": 1}
	for _, v := range pai.Variants {
		if got := v.Stock; got != querido[v.ID] {
			t.Errorf("variação %s veio com saldo %d, esperava %d — é o FÍSICO passando "+
				"direto, e vender a diferença é vender peça que já tem dono",
				v.ID, got, querido[v.ID])
		}
	}
}

// Com a regra DESLIGADA nada muda: o físico é o comportamento de antes, e
// inclusive sem a chamada extra ao Tiny.
func TestVariacaoMantemFisicoQuandoALojaNaoPede(t *testing.T) {
	saldos := map[string][2]int{
		"800000": {10, 2},
		"900001": {6, 1},
		"900002": {4, 3},
	}

	var consultasDeEstoque int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/estoque/") {
			consultasDeEstoque++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id": 800000, "nome": "Caneca", "tipo": "P",
			"precos": {"preco": 39.90},
			"estoque": {"quantidade": %d},
			"variacoes": [
				{"id": 900001, "descricao": "Vermelha", "estoque": {"quantidade": %d}, "precos": {"preco": 39.90}},
				{"id": 900002, "descricao": "Verde",    "estoque": {"quantidade": %d}, "precos": {"preco": 39.90}}
			]
		}`, saldos["800000"][0], saldos["900001"][0], saldos["900002"][0])
	}))
	defer srv.Close()

	original := tinyAPIBaseURL
	tinyAPIBaseURL = srv.URL
	defer func() { tinyAPIBaseURL = original }()

	tiny, err := NewTiny(TinyConfig{
		IntegrationID: "int-test",
		StoreID:       "store-test",
		Credentials:   &Credentials{AccessToken: "tok"},
		Logger:        zap.NewNop(),
		// UseAvailableStock ausente = desligado
	})
	if err != nil {
		t.Fatalf("NewTiny: %v", err)
	}

	pai, err := tiny.GetProduct(context.Background(), "800000")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if pai.Stock != 10 {
		t.Errorf("pai = %d, esperava o físico 10", pai.Stock)
	}
	for _, v := range pai.Variants {
		fisico := saldos[v.ID][0]
		if v.Stock != fisico {
			t.Errorf("variação %s = %d, esperava o físico %d", v.ID, v.Stock, fisico)
		}
	}
	if consultasDeEstoque != 0 {
		t.Errorf("houve %d consulta(s) a /estoque com a regra desligada — a loja "+
			"que não pediu não deve pagar a chamada extra", consultasDeEstoque)
	}
}

// Estoque indisponível para UMA variação não pode contaminar as outras nem
// derrubar o produto: aquela mantém o físico (comportamento de hoje quando não
// dá para afirmar), as demais recebem o disponível.
func TestVariacaoSemSaldoConsultavelMantemOFisico(t *testing.T) {
	saldos := map[string][2]int{
		"800000": {10, 2},
		"900001": {6, 1},
		// 900002 ausente de propósito: /estoque devolve 404.
	}
	srv := servidorDePaiComVariacoes(t, saldos)
	defer srv.Close()

	pai, err := tinyComSaldoDisponivel(t, srv).GetProduct(context.Background(), "800000")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	porID := map[string]int{}
	for _, v := range pai.Variants {
		porID[v.ID] = v.Stock
	}
	if porID["900001"] != 5 {
		t.Errorf("variação com estoque consultável = %d, esperava 5", porID["900001"])
	}
	if porID["900002"] != 0 {
		t.Errorf("variação sem estoque consultável = %d; o payload do pai a traz "+
			"com 0 e é esse número que deve sobrar", porID["900002"])
	}
}

// Guarda de forma: se o payload do pai deixar de trazer variações, o teste
// acima passaria por vazio. Esta asserção fixa a leitura do JSON.
func TestPayloadDoPaiRealmenteTrazVariacoes(t *testing.T) {
	var p tinyProductPayload
	const cru = `{"id":1,"nome":"x","tipo":"P","estoque":{"quantidade":7},
		"variacoes":[{"id":2,"descricao":"A","estoque":{"quantidade":3}}]}`
	if err := json.Unmarshal([]byte(cru), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Variacoes) != 1 {
		t.Fatalf("o parser deixou de ler variacoes[]: %d", len(p.Variacoes))
	}
	if p.Variacoes[0].Estoque.Quantidade != 3 {
		t.Errorf("saldo da variação lido como %v", p.Variacoes[0].Estoque.Quantidade)
	}
}
