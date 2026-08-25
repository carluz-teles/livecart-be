package erp

// Simetria entre reservar e estornar na classificação de falhas.
//
// O razão (`internal/erp/movement_ledger.go`) tem UMA tabela de classificação e
// ela é explícita: "reserva e estorno usam a mesma, porque a física é a mesma —
// só prova de não-entrega autoriza repetir". A tabela lê um único sinal do
// provider: `providers.ErrProvenUndelivered`.
//
// `ReserveStock` emite esse sinal em falha de discagem e em 4xx. `ReverseStockReservation`
// não emitia em nenhum caso, e a consequência apareceu em produção: um 429 —
// recusa por rate limit, o caso mais claramente NÃO-aplicado que existe — era
// classificado como `unconfirmed`, que nunca re-tenta e trava a finalização de
// um carrinho PAGO até alguém abrir o painel.
//
//	25/08/2026  movimento a62cf364, direction=in, carrinho #1115 (pago)
//	            last_error: "reverse stock reservation failed: status 429"
//
// Estes testes fixam a simetria: o que é repetível na saída é repetível na
// entrada, e o que é ambíguo continua ambíguo nos dois lados.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// respondeSempre devolve o mesmo status em toda chamada e conta quantas houve.
func respondeSempre(status int, corpo string, chamadas *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(chamadas, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(corpo))
	}))
}

// O caso que quebrou produção: 429 no estorno.
func TestEstornoCom429EhProvadoNaoAplicado(t *testing.T) {
	var chamadas int32
	srv := respondeSempre(http.StatusTooManyRequests, `{"mensagem":""}`, &chamadas)
	defer srv.Close()

	_, err := newTinyAgainst(t, srv).ReverseStockReservation(context.Background(), "357281337", 1, 0, "obs")
	if err == nil {
		t.Fatal("um 429 precisa virar erro")
	}
	if !errors.Is(err, providers.ErrProvenUndelivered) {
		t.Errorf("429 no estorno não foi marcado como provado não-aplicado: %v\n"+
			"sem o sentinela o razão classifica como 'unconfirmed', que nunca re-tenta "+
			"e trava a finalização de um carrinho pago", err)
	}
}

// Toda recusa 4xx é o Tiny dizendo não ANTES de aplicar — igual à reserva.
func TestEstornoCom4xxEhProvadoNaoAplicado(t *testing.T) {
	casos := []struct {
		nome   string
		status int
	}{
		{"400 validação", http.StatusBadRequest},
		{"403 sem permissão", http.StatusForbidden},
		{"404 produto inexistente", http.StatusNotFound},
		{"429 rate limit", http.StatusTooManyRequests},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var chamadas int32
			srv := respondeSempre(c.status, `{"mensagem":"recusado"}`, &chamadas)
			defer srv.Close()

			_, err := newTinyAgainst(t, srv).ReverseStockReservation(context.Background(), "357281337", 1, 0, "obs")
			if !errors.Is(err, providers.ErrProvenUndelivered) {
				t.Errorf("status %d não foi marcado como provado não-aplicado: %v", c.status, err)
			}
		})
	}
}

// 5xx continua ambíguo: o Tiny recebeu e pode ter dado entrada antes de quebrar.
// Repetir cegamente inflaria o saldo, que é o erro invisível e permanente.
func TestEstornoCom5xxContinuaAmbiguo(t *testing.T) {
	var chamadas int32
	srv := respondeSempre(http.StatusInternalServerError, `{"mensagem":"erro interno"}`, &chamadas)
	defer srv.Close()

	_, err := newTinyAgainst(t, srv).ReverseStockReservation(context.Background(), "357281337", 1, 0, "obs")
	if err == nil {
		t.Fatal("um 5xx precisa virar erro")
	}
	if errors.Is(err, providers.ErrProvenUndelivered) {
		t.Error("5xx no estorno foi marcado como provado não-aplicado — " +
			"o Tiny pode ter aplicado a entrada antes de quebrar, e repetir inflaria o saldo")
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes num 5xx; repetir dobraria a entrada", n)
	}
}

// Falha de discagem: nenhum byte chegou. Repetir é seguro e é o que a reserva
// já fazia — o estorno ia direto ao DoRequest cru, sem retry nenhum.
func TestEstornoRepeteFalhaDeDiscagem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // porta fechada ⇒ conexão recusada em toda tentativa

	fechado := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	fechado.URL = url

	_, err := newTinyAgainst(t, fechado).ReverseStockReservation(context.Background(), "357281337", 1, 0, "obs")
	if err == nil {
		t.Fatal("conexão recusada precisa virar erro")
	}
	if !errors.Is(err, providers.ErrProvenUndelivered) {
		t.Errorf("falha de discagem no estorno não foi marcada como repetível: %v", err)
	}
}

// O caminho feliz não muda: uma chamada, id do lançamento devolvido.
func TestEstornoBemSucedidoDevolveOLancamento(t *testing.T) {
	var chamadas int32
	srv := respondeSempre(http.StatusOK, `{"idLancamento":99012}`, &chamadas)
	defer srv.Close()

	id, err := newTinyAgainst(t, srv).ReverseStockReservation(context.Background(), "357281337", 1, 0, "obs")
	if err != nil {
		t.Fatalf("ReverseStockReservation: %v", err)
	}
	if id != "99012" {
		t.Errorf("id do lançamento = %q, esperava 99012", id)
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes num sucesso", n)
	}
}
