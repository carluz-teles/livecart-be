package live

// A DM não pode morrer com o prazo da requisição.
//
// Live de 17/08, 21:17. O Tiny estourou o prazo em duas adições, e a sequência
// no log foi:
//
//	failed to reserve stock in ERP        context deadline exceeded
//	failed to check notification settings context deadline exceeded
//
// A segunda linha é a consequência da primeira: a reserva no ERP roda antes da
// mensagem, no mesmo contexto, e consumiu o prazo. `amandinha2903` e `elima2013`
// tiveram item no carrinho sem receber aviso — e o retry do asynq não reenvia,
// porque o comentário já estava gravado e ele sai pelo portão de dedup.
//
// Toda a outra escrita deste fluxo pode ser retomada depois. A mensagem não.

import (
	"context"
	"testing"
	"time"
)

func TestContextoDaMensagemSobreviveAoPrazoVencido(t *testing.T) {
	// Uma requisição cujo prazo JÁ venceu — o estado exato das 21:17.
	vencido, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if vencido.Err() == nil {
		t.Fatal("o contexto de partida deveria estar vencido")
	}

	msg := contextoDaMensagem(vencido, "loja-1")
	if msg.Err() != nil {
		t.Errorf("a mensagem herdou o prazo vencido (%v) — é assim que o comprador "+
			"fica com item no carrinho e sem link para pagar", msg.Err())
	}
	if _, temPrazo := msg.Deadline(); temPrazo {
		t.Error("a mensagem carrega prazo da requisição; ela não deveria")
	}
}

// Cancelamento explícito de quem chamou também não derruba a mensagem.
func TestContextoDaMensagemNaoHerdaCancelamento(t *testing.T) {
	pai, cancel := context.WithCancel(context.Background())
	msg := contextoDaMensagem(pai, "loja-1")
	cancel()

	if msg.Err() != nil {
		t.Errorf("a mensagem foi cancelada junto com a requisição: %v", msg.Err())
	}
}
