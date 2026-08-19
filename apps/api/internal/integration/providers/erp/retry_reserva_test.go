package erp

// Retry na reserva de estoque: em quê se pode repetir, e em quê não.
//
// `POST /estoque/{id}` com tipo S CRIA um lançamento. Não é idempotente, e a API
// do Tiny não tem consulta de lançamentos — só criar e estornar. Então depois de
// uma falha não existe pergunta "chegou?" a fazer.
//
// Isso divide as falhas em duas classes com custos opostos:
//
//	discagem  conexão recusada, DNS, rede inalcançável. A requisição não chegou
//	          à aplicação. Repetir é seguro, e não repetir custa a reserva.
//	timeout   o Tiny pode ter dado baixa e demorado a responder. Repetir cria um
//	          SEGUNDO lançamento; o índice único de reserva ativa registra só um,
//	          e o outro fica órfão — estoque retirado do Tiny que ninguém comprou,
//	          que o estorno da expiração não devolve.
//
// Perder a reserva é detectável pela reconciliação. Reserva fantasma é invisível
// e permanente. Por isso o timeout NÃO é repetido aqui.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestFalhaDeDiscagemEhRepetivel(t *testing.T) {
	repetivel := []struct {
		nome string
		err  error
	}{
		{"conexão recusada", syscall.ECONNREFUSED},
		{"host inalcançável", syscall.EHOSTUNREACH},
		{"rede inalcançável", syscall.ENETUNREACH},
		{"DNS não resolveu", &net.DNSError{Err: "no such host", Name: "api.tiny.com.br"}},
		{"embrulhado", errors.Join(errors.New("dial tcp"), syscall.ECONNREFUSED)},
	}
	for _, c := range repetivel {
		t.Run(c.nome, func(t *testing.T) {
			if !falhaDeDiscagem(c.err) {
				t.Errorf("%v deveria ser repetível — a requisição não chegou ao Tiny "+
					"e não repetir custa a reserva", c.err)
			}
		})
	}
}

func TestTimeoutNaoEhRepetido(t *testing.T) {
	naoRepetivel := []struct {
		nome string
		err  error
	}{
		{"deadline do contexto", context.DeadlineExceeded},
		{"timeout de rede", &net.DNSError{Err: "i/o timeout", IsTimeout: true}},
		{"timeout embrulhado", errors.Join(errors.New("Post \"...\""), context.DeadlineExceeded)},
	}
	for _, c := range naoRepetivel {
		t.Run(c.nome, func(t *testing.T) {
			if falhaDeDiscagem(c.err) {
				t.Errorf("%v foi tratado como repetível — num timeout o Tiny pode ter "+
					"dado baixa, e repetir cria reserva fantasma que ninguém devolve", c.err)
			}
		})
	}
}

// Um 5xx é resposta, não falha de discagem: o Tiny recebeu e pode ter aplicado.
func TestRespostaDeErroNaoDisparaRetry(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chamadas, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"mensagem":"erro interno"}`))
	}))
	defer srv.Close()

	_, err := newTinyAgainst(t, srv).ReserveStock(context.Background(), "843169697", 2, 89.90, "obs")
	if err == nil {
		t.Fatal("erro do Tiny precisa subir")
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes; um 5xx é resposta e pode ter aplicado a baixa — "+
			"repetir dobraria o lançamento", n)
	}
}

// O caminho felizsegue igual: uma chamada, id do lançamento devolvido.
func TestReservaBemSucedidaNaoRepete(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chamadas, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"idLancamento":55501}`))
	}))
	defer srv.Close()

	id, err := newTinyAgainst(t, srv).ReserveStock(context.Background(), "843169697", 2, 89.90, "obs")
	if err != nil {
		t.Fatalf("ReserveStock: %v", err)
	}
	if id != "55501" {
		t.Errorf("id do lançamento = %q, esperava 55501", id)
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes num sucesso", n)
	}
}
