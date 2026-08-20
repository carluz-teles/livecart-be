package order_test

// Matéria-prima da árvore de histórico do pedido (20/08/2026): as DMs que o
// LiveCart mandou (notification_logs, com texto e desfecho) e a jornada
// COMPLETA da fila — inclusive entradas encerradas, que a seção "Aguardando
// estoque" de propósito esconde. O detalhe passa a carregar as duas.

import (
	"context"
	"testing"
	"time"

	"livecart/apps/api/internal/order"
)

func TestDetalheCarregaDMsEnviadasComDesfecho(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "HistDM")
	cartID := insertCart(t, eventID, "ana", "tok-hist-1", 9301, "pending", nil)

	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, event_id, cart_id, platform_user_id, notification_type, status, message_text, sent_at)
		 VALUES ($1, $2, $3, 'u-ana', 'checkout_immediate', 'sent', 'Olá! Seu carrinho: link', now())`,
		storeID, eventID, cartID); err != nil {
		t.Fatalf("seed DM enviada: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, event_id, cart_id, platform_user_id, notification_type, status, error_message)
		 VALUES ($1, $2, $3, 'u-ana', 'checkout_reminder', 'failed', 'janela de 24h fechada')`,
		storeID, eventID, cartID); err != nil {
		t.Fatalf("seed DM falhada: %v", err)
	}

	rows, err := order.NewRepository(testPool).ListCartNotifications(ctx, cartID)
	if err != nil {
		t.Fatalf("ListCartNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("DMs carregadas = %d; esperava 2", len(rows))
	}
	if rows[0].Type != "checkout_immediate" || rows[0].Status != "sent" ||
		rows[0].Message == nil || *rows[0].Message == "" {
		t.Errorf("primeira DM = %+v; esperava enviada COM o texto verbatim", rows[0])
	}
	if rows[1].Status != "failed" || rows[1].Error == nil {
		t.Errorf("segunda DM = %+v; a falha precisa vir com o motivo", rows[1])
	}
}

func TestDetalheCarregaJornadaCompletaDaFila(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "HistFila")
	produto := seedProduct(t, storeID, 1500)
	cartID := insertCart(t, eventID, "bia", "tok-hist-2", 9302, "pending", nil)

	ctx := context.Background()
	// Entrada ENCERRADA: liberou e o prazo venceu — o desfecho que hoje some
	// da tela (a fila ativa não a mostra mais).
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, cart_id, product_id, platform_user_id, platform_handle, quantity, position, status, notified_at, expires_at)
		 VALUES ($1, $2, $3, 'u-bia', '@bia', 2, 1, 'expired', now() - interval '2 hours', now() - interval '1 hour')`,
		eventID, cartID, produto); err != nil {
		t.Fatalf("seed fila expirada: %v", err)
	}

	rows, err := order.NewRepository(testPool).ListWaitlistJourney(ctx, cartID)
	if err != nil {
		t.Fatalf("ListWaitlistJourney: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("jornada = %d entradas; esperava 1 (a encerrada TEM de aparecer)", len(rows))
	}
	w := rows[0]
	if w.Status != "expired" || w.NotifiedAt == nil || w.ExpiresAt == nil {
		t.Errorf("jornada = %+v; esperava expired com notified_at e expires_at", w)
	}
	if w.NotifiedAt != nil && time.Since(*w.NotifiedAt) < time.Hour {
		t.Errorf("notified_at = %v; esperava ~2h atrás", w.NotifiedAt)
	}
	if w.Quantity != 2 || w.ProductName == "" {
		t.Errorf("jornada sem produto/quantidade: %+v", w)
	}
}
