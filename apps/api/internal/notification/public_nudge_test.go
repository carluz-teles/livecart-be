package notification

// O convite público, e por que ele existe.
//
// Medido no banco de produção em 09/08/2026, sem uma exceção: dos 61
// comentários capturados por POLLING, zero aceitaram private reply. A Meta só
// aceita `POST /{comment_id}/private_replies` para um comment_id que ela mesma
// entregou por webhook. O fallback seguinte — DM direta — bate em 403/2534022
// ("outside of allowed window"), porque comentário não abre a janela de 24h.
//
// Resultado antes desta mudança: enquanto o webhook de live_comments está mudo
// (5 a 44 segundos após o início de CADA transmissão medida), o carrinho nascia
// e o comprador nunca recebia o link. O painel mostrava pedido criado e a venda
// morria em silêncio.
//
// A resposta pública é o único canal que sobra. O que ela NÃO leva é o ponto:
// ver TestConviteNaoPodeLevarOLinkDoCarrinho.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCompradorEhChamadoAoDirect(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	// Os dois caminhos privados recusados — o estado real de toda live medida
	// depois que o webhook emudece.
	sender := &fakeDMSender{failWith: errors.New("status 400, body: (#2534022) fora da janela")}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.CommentCreatedAt = time.Now().Add(-1 * time.Hour)

	res, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sender.publicCalls != 1 {
		t.Fatalf("resposta pública chamada %d vezes, quero 1 — sem ela o comprador "+
			"não fica sabendo que tem carrinho", sender.publicCalls)
	}
	if res.Reason != ReasonNudgedPublicly {
		t.Errorf("motivo = %q, quero %q", res.Reason, ReasonNudgedPublicly)
	}
	// 'undelivered' e não 'sent': o que foi público é o convite, não a mensagem.
	// Marcar como entregue esconderia do lojista um comprador que ainda não tem
	// o link — que é exatamente a lista que a RN-38 existe para mostrar.
	if res.Status != StatusUndelivered {
		t.Errorf("status = %q, quero %q", res.Status, StatusUndelivered)
	}

	status, reason, _ := readLog(t, res.LogID)
	if status != string(StatusUndelivered) || reason == nil || *reason != string(ReasonNudgedPublicly) {
		t.Errorf("log gravado = (%q, %v), quero (%q, %q)",
			status, reason, StatusUndelivered, ReasonNudgedPublicly)
	}
}

// A regra que não pode ser relaxada por conveniência.
//
// /cart/<token> é uma URL-capacidade: quem tem o endereço abre o carrinho,
// altera dados e finaliza o pedido, sem login. Publicá-la sob o comentário
// entregaria o carrinho do comprador para a live inteira.
func TestConviteNaoPodeLevarOLinkDoCarrinho(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{failWith: errors.New("(#2534022) fora da janela")}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.CommentCreatedAt = time.Now().Add(-1 * time.Hour)

	if _, err := svc.Send(ctx, in); err != nil {
		t.Fatalf("Send: %v", err)
	}

	texto := sender.publicText
	if texto == "" {
		t.Fatal("nenhum texto público capturado")
	}
	for _, proibido := range []string{"/cart/", "http://", "https://"} {
		if strings.Contains(texto, proibido) {
			t.Errorf("texto público contém %q — isso expõe o carrinho do comprador "+
				"para qualquer um que leia a live.\ntexto: %s", proibido, texto)
		}
	}
	// E precisa dizer o que fazer, senão o convite não converte em nada.
	if !strings.Contains(strings.ToLower(texto), "direct") {
		t.Errorf("texto público não chama o comprador ao direct: %s", texto)
	}
}

// O público é ÚLTIMO recurso. Se o private reply passa, nada vai para a timeline
// — a mensagem tem o link e a conversa é privada por padrão.
func TestConviteNaoDisparaQuandoOPrivadoFunciona(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{} // tudo passa
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.CommentCreatedAt = time.Now().Add(-1 * time.Hour)

	res, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != StatusSent {
		t.Fatalf("status = %q, quero sent", res.Status)
	}
	if sender.publicCalls != 0 {
		t.Errorf("resposta pública disparou %d vezes com o privado funcionando — "+
			"isso vaza para a timeline sem necessidade", sender.publicCalls)
	}
}

// Sem comentário para responder (venda por story/DM) não existe onde publicar.
// O caminho tem de continuar sendo o DM direto, e o público não pode ser
// tentado com um comment_id vazio.
func TestSemComentarioNaoTentaRespostaPublica(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{failWith: errors.New("(#2534022) fora da janela")}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.PlatformCommentID = ""
	in.DirectOnly = true

	if _, err := svc.Send(ctx, in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sender.publicCalls != 0 {
		t.Errorf("tentou responder publicamente sem comentário (%d chamadas)", sender.publicCalls)
	}
	if sender.dmCalls == 0 {
		t.Error("não tentou o DM direto, que é o único caminho da venda por story")
	}
}

// O texto do convite mencionando o @ do comprador — sem o @, numa live com
// dezenas de comentários por minuto, ninguém sabe que a mensagem é para si.
func TestConviteMencionaOComprador(t *testing.T) {
	if got := publicNudgeText("ana"); !strings.Contains(got, "@ana") {
		t.Errorf("convite não menciona o comprador: %s", got)
	}
	// Sem handle o texto ainda precisa fazer sentido — @ solto viraria menção
	// quebrada na timeline.
	semHandle := publicNudgeText("")
	if strings.Contains(semHandle, "@") {
		t.Errorf("convite sem handle não pode ter @ solto: %s", semHandle)
	}
	if semHandle == "" {
		t.Error("convite sem handle ficou vazio")
	}
}
