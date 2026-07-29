package postcheckout_test

// Fatia A1 — o recibo do comprador (SendOrderPaid) e o e-mail de estorno
// (SendOrderRefunded) saíram do fan-out inline do postcheckout e viraram
// workers do reactor de Notification. Estes testes travam o comportamento de
// SendPaidReceipt / SendRefundEmail (onde a lógica de guarda + idempotência
// vive) e provam que OnCartPaid NÃO envia mais o recibo inline. Gated em
// TEST_DATABASE_URL (mesma infra dos testes fatia_b1/C1). Usa um EmailSender
// fake — nenhum e-mail real é disparado.
//
// Invariantes:
//   A  SendPaidReceipt envia o recibo EXATAMENTE 1× e SÓ com Order materializada
//      + tracking_token setado.
//   A-guard  sem Order materializada OU sem token → ErrReceiptNotReady (retry),
//            nenhum e-mail.
//   B  redelivery (2ª chamada) NÃO reenvia (marca receipt_sent unique).
//   C  SendRefundEmail envia o estorno 1×; redelivery não reenvia.
//   D  postcheckout.OnCartPaid NÃO envia mais o recibo (send saiu de lá).

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/postcheckout"
	"livecart/apps/api/lib/email"
)

// fakeEmailSender conta os envios e captura os inputs, sem tocar a rede. Satisfaz
// postcheckout.EmailSender.
type fakeEmailSender struct {
	paid      []email.OrderPaidEmailInput
	refunded  []email.OrderRefundedEmailInput
	cancelled []email.OrderCancelledEmailInput
	delivered []email.OrderDeliveredEmailInput
	shipped   []email.OrderShippedEmailInput
}

func (f *fakeEmailSender) SendOrderPaid(_ context.Context, in email.OrderPaidEmailInput) error {
	f.paid = append(f.paid, in)
	return nil
}
func (f *fakeEmailSender) SendOrderRefunded(_ context.Context, in email.OrderRefundedEmailInput) error {
	f.refunded = append(f.refunded, in)
	return nil
}
func (f *fakeEmailSender) SendOrderCancelled(_ context.Context, in email.OrderCancelledEmailInput) error {
	f.cancelled = append(f.cancelled, in)
	return nil
}
func (f *fakeEmailSender) SendOrderDelivered(_ context.Context, in email.OrderDeliveredEmailInput) error {
	f.delivered = append(f.delivered, in)
	return nil
}
func (f *fakeEmailSender) SendOrderShipped(_ context.Context, in email.OrderShippedEmailInput) error {
	f.shipped = append(f.shipped, in)
	return nil
}

// newServiceWithFake monta o Service com um EmailSender fake para observar envios.
func newServiceWithFake(t *testing.T) (*postcheckout.Service, *fakeEmailSender) {
	t.Helper()
	fake := &fakeEmailSender{}
	svc := postcheckout.NewService(postcheckout.NewRepository(testQueries), fake, zap.NewNop())
	return svc, fake
}

// setCustomerEmail seta o e-mail do comprador no cart (os seeds nascem sem ele
// para pular envio nos testes de token/timeline).
func setCustomerEmail(t *testing.T, ctx context.Context, cartID, e string) {
	t.Helper()
	if _, err := testPool.Exec(ctx, `UPDATE carts SET customer_email = $2 WHERE id = $1`, cartID, e); err != nil {
		t.Fatalf("set customer_email: %v", err)
	}
}

func countOrderEvents(t *testing.T, ctx context.Context, cartID, eventType string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events WHERE cart_id = $1 AND event_type = $2`, cartID, eventType,
	).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

// ─── A: envia 1× só com Order materializada + token ───────────────────────────

func TestSendPaidReceipt_A_SendsOnceWhenOrderAndTokenReady(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	svc, fake := newServiceWithFake(t)
	// OnCartPaid materializa o tracking_token (não envia e-mail — Fatia A1).
	svc.OnCartPaid(ctx, cartID)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	if err := svc.SendPaidReceipt(ctx, cartID); err != nil {
		t.Fatalf("A: SendPaidReceipt returned error: %v", err)
	}
	if len(fake.paid) != 1 {
		t.Fatalf("A: expected exactly 1 receipt sent, got %d", len(fake.paid))
	}
	if fake.paid[0].ToEmail != "buyer@example.com" {
		t.Errorf("A: receipt to wrong address: %q", fake.paid[0].ToEmail)
	}
	if fake.paid[0].TrackingURL == "" {
		t.Errorf("A: receipt has empty tracking URL (token não propagou)")
	}
}

// ─── A-guard: sem Order/token → ErrReceiptNotReady, sem envio ──────────────────

func TestSendPaidReceipt_AGuard_NotMaterialisedRetries(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID, _ := seedPaidCart(t) // Order NÃO materializada
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	svc, fake := newServiceWithFake(t)
	err := svc.SendPaidReceipt(ctx, cartID)
	if !errors.Is(err, postcheckout.ErrReceiptNotReady) {
		t.Fatalf("A-guard: expected ErrReceiptNotReady, got %v", err)
	}
	if len(fake.paid) != 0 {
		t.Errorf("A-guard: no receipt should be sent before Order is ready, got %d", len(fake.paid))
	}
}

func TestSendPaidReceipt_AGuard_TokenNotSetRetries(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	// Order materializada mas OnCartPaid ainda não rodou → tracking_token NULL.
	cartID := seedPaidCartWithOrder(t)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	svc, fake := newServiceWithFake(t)
	err := svc.SendPaidReceipt(ctx, cartID)
	if !errors.Is(err, postcheckout.ErrReceiptNotReady) {
		t.Fatalf("A-guard: expected ErrReceiptNotReady when token unset, got %v", err)
	}
	if len(fake.paid) != 0 {
		t.Errorf("A-guard: no receipt should be sent with unset token, got %d", len(fake.paid))
	}
}

// ─── B: redelivery não reenvia ────────────────────────────────────────────────

func TestSendPaidReceipt_B_IdempotentOnRedelivery(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	svc, fake := newServiceWithFake(t)
	svc.OnCartPaid(ctx, cartID)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	for i := 0; i < 3; i++ {
		if err := svc.SendPaidReceipt(ctx, cartID); err != nil {
			t.Fatalf("B: SendPaidReceipt #%d error: %v", i, err)
		}
	}
	if len(fake.paid) != 1 {
		t.Errorf("B: expected 1 receipt após 3× SendPaidReceipt, got %d", len(fake.paid))
	}
	if c := countOrderEvents(t, ctx, cartID, "receipt_sent"); c != 1 {
		t.Errorf("B: expected 1 receipt_sent marker, got %d", c)
	}
}

// ─── C: estorno envia 1×, idempotente ─────────────────────────────────────────

func TestSendRefundEmail_C_SendsOnceAndIdempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	svc, fake := newServiceWithFake(t)
	for i := 0; i < 2; i++ {
		if err := svc.SendRefundEmail(ctx, cartID); err != nil {
			t.Fatalf("C: SendRefundEmail #%d error: %v", i, err)
		}
	}
	if len(fake.refunded) != 1 {
		t.Errorf("C: expected 1 refund email após 2× SendRefundEmail, got %d", len(fake.refunded))
	}
	if c := countOrderEvents(t, ctx, cartID, "payment_refunded"); c != 1 {
		t.Errorf("C: expected 1 payment_refunded marker, got %d", c)
	}
}

// ─── D: OnCartPaid NÃO envia mais o recibo inline ─────────────────────────────

func TestOnCartPaid_D_NoLongerSendsReceiptInline(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	svc, fake := newServiceWithFake(t)
	// Mesmo com Order materializada, token gerado aqui e customer_email presente,
	// OnCartPaid não pode disparar o recibo — isso agora é do reactor Notification.
	svc.OnCartPaid(ctx, cartID)

	if len(fake.paid) != 0 {
		t.Errorf("D: OnCartPaid não deveria enviar recibo inline (send saiu p/ o reactor); got %d", len(fake.paid))
	}
	// Sem receipt_sent — o marcador é responsabilidade de SendPaidReceipt.
	if c := countOrderEvents(t, ctx, cartID, "receipt_sent"); c != 0 {
		t.Errorf("D: OnCartPaid não deveria gravar receipt_sent, got %d", c)
	}
}
