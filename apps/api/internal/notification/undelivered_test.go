package notification

// RN-38 — não entrega COM MOTIVO, contra Postgres real.
//
// O que se prova aqui é a diferença entre as duas coisas que antes eram
// indistinguíveis: "o Instagram recusou" e "nem tentamos porque a janela de 7
// dias já tinha fechado". As duas produziam um status 'failed' com o erro cru
// do Graph em error_message — texto que não se mostra ao lojista, não se agrupa
// por motivo e não vira lista de "quem eu chamo na mão".

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeDMSender registra as tentativas e pode falhar sob demanda. Contar
// tentativas é o ponto: a mensagem fora da janela não pode gerar NENHUMA.
type fakeDMSender struct {
	dmCalls    int
	replyCalls int
	failWith   error
}

func (f *fakeDMSender) SendInstagramDM(_ context.Context, _, _, _ string) error {
	f.dmCalls++
	return f.failWith
}

func (f *fakeDMSender) ReplyToInstagramComment(_ context.Context, _, _, _ string) error {
	f.replyCalls++
	return f.failWith
}

func seedStoreEventCart(t *testing.T) (storeID, eventID, cartID string) {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("undeliv-%d", time.Now().UnixNano())

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Loja RN38', $1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type)
		 VALUES ($1::uuid, 'ended', 'Semana Black', 'multi') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, token, short_id, platform_user_id, platform_handle, status)
		 VALUES ($1::uuid, $2, $3, 'ig-42', 'ana', 'checkout') RETURNING id::text`,
		eventID, "tok-"+slug, shortIDSeq(),
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	return storeID, eventID, cartID
}

// short_id é NOT NULL e o gerador de produção é um UPSERT por loja; no teste
// basta um número único.
var shortIDCounter int32 = 1000

func shortIDSeq() int32 { return atomic.AddInt32(&shortIDCounter, 1) }

func readLog(t *testing.T, logID string) (status string, reason *string, sentAt *time.Time) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, undelivered_reason, sent_at FROM notification_logs WHERE id = $1::uuid`, logID,
	).Scan(&status, &reason, &sentAt); err != nil {
		t.Fatalf("readLog: %v", err)
	}
	return status, reason, sentAt
}

func sendInput(storeID, eventID, cartID string) SendInput {
	return SendInput{
		StoreID:           storeID,
		EventID:           eventID,
		CartID:            cartID,
		PlatformUserID:    "ig-42",
		PlatformHandle:    "ana",
		PlatformCommentID: "cmt-1",
		NotificationType:  TypeEventDeadlineStarted,
		Variables:         SampleVariables(),
	}
}

func TestJanelaFechadaNaoTentaEEscreveOMotivo(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	// Oito dias: numa campanha de uma semana este é o comprador do primeiro
	// dia, e é para ele que a mensagem de maior conversão dispara.
	in.CommentCreatedAt = time.Now().Add(-8 * 24 * time.Hour)

	res, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if res.Status != StatusUndelivered {
		t.Errorf("status = %q, quero %q", res.Status, StatusUndelivered)
	}
	if res.Reason != ReasonCommentWindowExpired {
		t.Errorf("motivo = %q, quero %q", res.Reason, ReasonCommentWindowExpired)
	}
	// Nenhuma chamada ao Instagram: a porta já estava fechada e tentar seria
	// gastar API para receber 2534022 e registrar isso como falha genérica.
	if sender.replyCalls != 0 || sender.dmCalls != 0 {
		t.Errorf("tentou enviar mesmo fora da janela (reply=%d, dm=%d)", sender.replyCalls, sender.dmCalls)
	}

	status, reason, sentAt := readLog(t, res.LogID)
	if status != string(StatusUndelivered) {
		t.Errorf("status gravado = %q", status)
	}
	if reason == nil || *reason != string(ReasonCommentWindowExpired) {
		t.Errorf("undelivered_reason gravado = %v", reason)
	}
	// sent_at NULL é a metade que impede a ilusão de entrega: uma linha com
	// horário de envio e nada enviado é pior do que nenhuma linha.
	if sentAt != nil {
		t.Errorf("sent_at = %v, quero NULL — nada foi enviado", sentAt)
	}
}

func TestDentroDaJanelaEnviaNormalmente(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.CommentCreatedAt = time.Now().Add(-6 * 24 * time.Hour) // borda: ainda vale

	res, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != StatusSent {
		t.Fatalf("status = %q, quero sent", res.Status)
	}
	if sender.replyCalls != 1 {
		t.Errorf("private reply chamado %d vezes, quero 1", sender.replyCalls)
	}

	_, reason, _ := readLog(t, res.LogID)
	if reason != nil {
		t.Errorf("undelivered_reason = %v numa entrega bem-sucedida", *reason)
	}
}

func TestCarimboDesconhecidoTenta(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	// Sem CommentCreatedAt: errar para o lado de TENTAR. Silenciar por falta de
	// dado seria pior do que uma chamada perdida.
	res, err := svc.Send(ctx, sendInput(storeID, eventID, cartID))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != StatusSent {
		t.Errorf("status = %q, quero sent", res.Status)
	}
	if sender.replyCalls == 0 {
		t.Error("não tentou enviar sem carimbo de tempo")
	}
}

func TestRecusaDoInstagramFicaFailedMasEntraNaLista(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{failWith: errors.New("status 400, body: (#2534022) fora da janela")}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeID, eventID, cartID)
	in.CommentCreatedAt = time.Now().Add(-1 * time.Hour)

	res, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, quero failed — houve tentativa real", res.Status)
	}

	status, reason, _ := readLog(t, res.LogID)
	if status != string(StatusFailed) {
		t.Errorf("status gravado = %q, quero failed", status)
	}
	if reason == nil || *reason != string(ReasonInstagramRejected) {
		t.Fatalf("undelivered_reason = %v, quero %q", reason, ReasonInstagramRejected)
	}

	// A linha recusada TEM de aparecer na lista do lojista, mesmo com status
	// 'failed': o filtro da lista é a coluna de motivo, não o status.
	entries, err := svc.ListUndelivered(ctx, storeID, eventID)
	if err != nil {
		t.Fatalf("ListUndelivered: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("lista tem %d entradas, quero 1", len(entries))
	}
	if entries[0].ReasonText == "" {
		t.Error("entrada sem texto de motivo — o painel não teria o que mostrar")
	}
	if entries[0].CartToken == "" {
		t.Error("entrada sem token do carrinho — o lojista não consegue copiar o link")
	}
}

func TestListaColapsaPorCompradorENaoPorTentativa(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID, cartID := seedStoreEventCart(t)
	sender := &fakeDMSender{}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	// A mesma pessoa fica fora da janela para TUDO que a campanha dispara:
	// listar cada tentativa transformaria "quem eu chamo na mão" num log.
	for _, notifType := range []NotificationType{TypeEventDeadlineStarted, TypeWaitlistUnfulfilled, TypeOutOfWindowEventEnded} {
		in := sendInput(storeID, eventID, cartID)
		in.NotificationType = notifType
		in.CommentCreatedAt = time.Now().Add(-9 * 24 * time.Hour)
		if _, err := svc.Send(ctx, in); err != nil {
			t.Fatalf("Send(%s): %v", notifType, err)
		}
	}

	entries, err := svc.ListUndelivered(ctx, storeID, eventID)
	if err != nil {
		t.Fatalf("ListUndelivered: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("lista tem %d entradas para o mesmo comprador, quero 1", len(entries))
	}
	if entries[0].PlatformHandle != "ana" {
		t.Errorf("handle = %q", entries[0].PlatformHandle)
	}
}

func TestListaEhEscopadaPelaLoja(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeA, eventA, cartA := seedStoreEventCart(t)
	storeB, _, _ := seedStoreEventCart(t)

	sender := &fakeDMSender{}
	svc := &Service{queries: testQueries, dmSender: sender, logger: zap.NewNop()}

	in := sendInput(storeA, eventA, cartA)
	in.CommentCreatedAt = time.Now().Add(-9 * 24 * time.Hour)
	if _, err := svc.Send(ctx, in); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Outra loja pedindo o mesmo eventId não pode ver a lista alheia.
	entries, err := svc.ListUndelivered(ctx, storeB, eventA)
	if err != nil {
		t.Fatalf("ListUndelivered: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("loja B enxergou %d entradas da loja A", len(entries))
	}
}
