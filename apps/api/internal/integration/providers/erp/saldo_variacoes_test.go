package erp

// O saldo que vale para vender é o DISPONÍVEL — pai e variações, sem exceção
// e sem configuração.
//
// `GET /produtos/{id}` devolve `estoque.quantidade`, o saldo FÍSICO, e esse
// número conta peça que já tem dono: cada unidade presa num pedido de venda
// aberto continua ali. Oferecê-la é vender duas vezes a mesma coisa — e foi
// assim que a cantodaart vendeu o que não tinha.
//
// O disponível vem de `/estoque/{id}`, uma consulta por id. Quando não dá para
// afirmar, o produto volta com StockKnown falso e quem espelha não escreve —
// nem zera, nem cai no físico.

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

// tinyComSaldoDisponivel monta um provider apontado para o servidor de teste.
func tinyComSaldoDisponivel(t *testing.T, srv *httptest.Server) *Tiny {
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

func TestPaiEVariacoesUsamOSaldoDisponivel(t *testing.T) {
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

// O FÍSICO nunca vaza para o resultado, nem quando é o único número legível.
//
// Este é o teste que substituiu o antigo "com a regra desligada mantém o
// físico". Aquele fixava justamente o comportamento que causava o furo; agora o
// contrato é o oposto e precisa ser afirmado: `/estoque` fora do ar não
// autoriza ninguém a usar `estoque.quantidade`.
func TestFisicoNuncaViraSaldoQuandoODisponivelFalha(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/estoque/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, `{
			"id": 800000, "nome": "Caneca", "tipo": "P",
			"precos": {"preco": 39.90},
			"estoque": {"quantidade": 10},
			"variacoes": [
				{"id": 900001, "descricao": "Vermelha", "estoque": {"quantidade": 6}, "precos": {"preco": 39.90}}
			]
		}`)
	}))
	defer srv.Close()

	pai, err := tinyComSaldoDisponivel(t, srv).GetProduct(context.Background(), "800000")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if pai.StockKnown {
		t.Errorf("StockKnown = true com /estoque fora do ar — o chamador vai " +
			"escrever um número que ninguém apurou")
	}
	if pai.Stock == 10 {
		t.Errorf("saldo do pai = 10, que é o FÍSICO de `estoque.quantidade` — " +
			"é exatamente o vazamento que esta regra existe para impedir")
	}
	for _, v := range pai.Variants {
		if v.StockKnown {
			t.Errorf("variação %s veio com StockKnown = true sem saldo apurável", v.ID)
		}
		if v.Stock == 6 {
			t.Errorf("variação %s = 6, o físico do payload do pai", v.ID)
		}
	}
}

// Estoque indisponível para UMA variação não contamina as outras nem derruba o
// produto: aquela volta marcada como não-apurada, as demais com o disponível.
func TestVariacaoSemSaldoConsultavelFicaMarcadaComoNaoApurada(t *testing.T) {
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

	porID := map[string]ERPProduct{}
	for _, v := range pai.Variants {
		porID[v.ID] = v
	}
	if v := porID["900001"]; !v.StockKnown || v.Stock != 5 {
		t.Errorf("variação com estoque consultável = %d (conhecido=%v), esperava 5 apurado",
			v.Stock, v.StockKnown)
	}
	if v := porID["900002"]; v.StockKnown {
		t.Errorf("variação sem estoque consultável voltou como apurada (%d) — o "+
			"físico 4 do payload do pai não pode virar saldo", v.Stock)
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
