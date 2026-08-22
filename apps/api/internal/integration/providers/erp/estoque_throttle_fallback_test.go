package erp

// 429 no GET /estoque NÃO pode virar saldo físico — incidente 834962410
// (22/08/2026).
//
// A cantodaart cadastrou dois produtos quase idênticos em sequência. O
// 834962410 pegou um 429 no GET /estoque (uma varredura de estoque concorrente
// consumiu a cota de ~1 req/s do Tiny no mesmo segundo), e como saldoDisponivel
// não re-tentava, caiu no saldo FÍSICO — exatamente o que a opção "apenas
// disponível" existe para impedir. O 837156336, 2s depois, passou sem 429 e
// puxou o disponível certo. Não era o produto: era estrangulamento transitório.
//
// A correção re-tenta o 429 com backoff curto antes de desistir. Só cai no
// físico se o estrangulamento persistir — 404 (sem controle de estoque) segue
// caindo na hora, sem espera.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// backoffInstantaneo encurta a espera entre retentativas para o teste não
// dormir os ~1.2s de produção.
func backoffInstantaneo(t *testing.T) {
	t.Helper()
	original := estoqueThrottleBackoff
	estoqueThrottleBackoff = time.Millisecond
	t.Cleanup(func() { estoqueThrottleBackoff = original })
}

// TestEstoque429TransitorioNaoCaiNoFisico: o /estoque estrangula nas 2
// primeiras chamadas e só então devolve o disponível. GetProduct tem de
// re-tentar e gravar o DISPONÍVEL (1), nunca o físico (3).
func TestEstoque429TransitorioNaoCaiNoFisico(t *testing.T) {
	backoffInstantaneo(t)

	var tentativasEstoque int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/estoque/") {
			n := atomic.AddInt32(&tentativasEstoque, 1)
			if n <= 2 { // as duas primeiras estrangulam
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			// físico 3, reservado 2 -> disponível 1 (o número do caso real)
			_, _ = fmt.Fprint(w, `{"saldo":3,"reservado":2,"disponivel":1}`)
			return
		}
		// GET /produtos/{id}: traz o FÍSICO (3) em estoque.quantidade.
		_, _ = fmt.Fprint(w, `{"id":834962410,"nome":"Caixa de Correio Gingerbread","tipo":"P","precos":{"preco":49.90},"estoque":{"quantidade":3}}`)
	}))
	defer srv.Close()

	prod, err := tinyComSaldoDisponivel(t, srv).GetProduct(context.Background(), "834962410")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if prod.Stock != 1 {
		t.Errorf("Stock = %d, quero 1 (disponível). 3 seria o físico passando pelo 429 — "+
			"o furo do 834962410", prod.Stock)
	}
	if got := atomic.LoadInt32(&tentativasEstoque); got != 3 {
		t.Errorf("tentativas ao /estoque = %d, quero 3 (2 estranguladas + 1 boa) — o retry não rodou", got)
	}
}

// TestEstoque404CaiNoFisicoSemEsperar: 404 é ausência real (produto sem
// controle de estoque), não estrangulamento — cai no físico de imediato, uma
// só chamada, sem retry.
func TestEstoque404CaiNoFisicoSemEsperar(t *testing.T) {
	backoffInstantaneo(t)

	var tentativasEstoque int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/estoque/") {
			atomic.AddInt32(&tentativasEstoque, 1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":555,"nome":"Sem controle de estoque","tipo":"P","precos":{"preco":10.0},"estoque":{"quantidade":7}}`)
	}))
	defer srv.Close()

	prod, err := tinyComSaldoDisponivel(t, srv).GetProduct(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if prod.Stock != 7 {
		t.Errorf("Stock = %d, quero 7 (físico) — 404 sem controle de estoque preserva o físico", prod.Stock)
	}
	if got := atomic.LoadInt32(&tentativasEstoque); got != 1 {
		t.Errorf("tentativas ao /estoque = %d, quero 1 — 404 não pode ser re-tentado como se fosse 429", got)
	}
}
