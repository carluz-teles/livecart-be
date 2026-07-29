package integration

// CANCELAMENTO DE CARRINHO PELO LOJISTA (LIV-84).
//
// Comportamento coberto aqui:
//   1. cancelar devolve o estoque, mata a fila do próprio cart e marca o cart
//      como 'cancelled' com reason='store_cancelled' (≠ 'expired', que é o que
//      o comprador vê como "acabou o prazo");
//   2. cart PAGO não pode ser cancelado — o guard do UPDATE recusa e nada de
//      estoque se move (esta é a corrida "pagou antes do clique");
//   3. cancelar duas vezes não devolve estoque duas vezes (idempotência);
//   4. o pagamento que chega DEPOIS do cancelamento VENCE: o cart volta como
//      pago, o estoque é retomado e as colunas de ERP são zeradas para uma
//      finalização limpa;
//   5. o restore só vale para cancelamento do LOJISTA — cart expirado ou
//      cancelado por bloqueio de comprador não ressuscita.
//
// Rodar:
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestCancel -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	notificationinbox "livecart/apps/api/internal/notification_inbox"
)

func cartStatusAndReason(t *testing.T, cartID string) (status, reason string) {
	t.Helper()
	var r *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, cancelled_reason FROM carts WHERE id = $1`, cartID).Scan(&status, &r); err != nil {
		t.Fatalf("lendo cart: %v", err)
	}
	if r != nil {
		reason = *r
	}
	return status, reason
}

func markCartPaid(t *testing.T, cartID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET payment_status = 'paid', paid_at = now() WHERE id = $1`, cartID); err != nil {
		t.Fatalf("marcando cart pago: %v", err)
	}
}

// Cancelar um carrinho aberto devolve o estoque das unidades que ele segurava,
// encerra a fila vinculada a ele e deixa o cart legível como "cancelado pela
// loja" — não como expirado.
func TestCancelCartReleasesStockAndMarksReason(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 3)

	if got := productStock(t, productID); got != 0 {
		t.Fatalf("pré-condição: estoque deveria ser 0, veio %d", got)
	}

	if err := scaleService().CancelCart(ctx, cartID, fx.storeID); err != nil {
		t.Fatalf("CancelCart: %v", err)
	}

	status, reason := cartStatusAndReason(t, cartID)
	if status != "cancelled" {
		t.Errorf("status = %q, queria \"cancelled\"", status)
	}
	if reason != CancelReasonStore {
		t.Errorf("cancelled_reason = %q, queria %q", reason, CancelReasonStore)
	}
	if got := productStock(t, productID); got != 3 {
		t.Errorf("estoque devolvido = %d, queria 3", got)
	}
}

// A fila de espera do carrinho cancelado morre junto: promover alguém para um
// checkout que não aceita mais pagamento só geraria DM para uma porta fechada.
func TestCancelCartKillsItsOwnWaitlistEntries(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	waiterCart := seedQueueWaiter(t, fx, productID, 1)

	if err := scaleService().CancelCart(ctx, waiterCart, fx.storeID); err != nil {
		t.Fatalf("CancelCart: %v", err)
	}

	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM waitlist_items WHERE cart_id = $1`, waiterCart).Scan(&status); err != nil {
		t.Fatalf("lendo waitlist_item: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("waitlist_item.status = %q, queria \"cancelled\"", status)
	}
}

// Corrida: o cliente pagou entre a abertura da tela e o clique em cancelar. O
// guard do UPDATE recusa, o lojista recebe 409 e NADA de estoque se move — a
// unidade continua vendida.
func TestCancelCartRefusesPaidCart(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 2)
	markCartPaid(t, cartID)

	err := scaleService().CancelCart(ctx, cartID, fx.storeID)
	if err == nil {
		t.Fatal("CancelCart deveria recusar um cart pago")
	}

	status, _ := cartStatusAndReason(t, cartID)
	if status == "cancelled" {
		t.Error("cart pago foi marcado como cancelado")
	}
	if got := productStock(t, productID); got != 0 {
		t.Errorf("estoque de venda paga foi devolvido: %d", got)
	}
}

// Idempotência: um segundo cancelamento (clique duplo, retry de rede) não pode
// devolver estoque de novo.
func TestCancelCartTwiceReleasesStockOnce(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 2)
	svc := scaleService()

	if err := svc.CancelCart(ctx, cartID, fx.storeID); err != nil {
		t.Fatalf("primeiro CancelCart: %v", err)
	}
	if err := svc.CancelCart(ctx, cartID, fx.storeID); err == nil {
		t.Error("segundo CancelCart deveria recusar (cart já cancelado)")
	}

	if got := productStock(t, productID); got != 2 {
		t.Errorf("estoque = %d, queria 2 (devolução única)", got)
	}
}

// Corrida inversa e regra central: o pagamento aprovado chega DEPOIS do
// cancelamento. O pedido volta e consta como PAGO, o estoque é retomado (mesmo
// que fique negativo — a venda existe) e o estado de ERP é zerado para que a
// finalização crie um pedido novo e limpo no Tiny.
func TestPaymentAfterCancellationWins(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)

	if err := scaleService().CancelCart(ctx, cartID, fx.storeID); err != nil {
		t.Fatalf("CancelCart: %v", err)
	}
	if got := productStock(t, productID); got != 1 {
		t.Fatalf("pré-condição: estoque devolvido = %d, queria 1", got)
	}

	paidAt := time.Now().UTC()
	restored, err := testRepo.RestoreCancelledCartAsPaid(ctx, cartID, fx.storeID, "paid", "pay_123", &paidAt, "pix")
	if err != nil {
		t.Fatalf("RestoreCancelledCartAsPaid: %v", err)
	}
	if !restored {
		t.Fatal("restored = false, queria true")
	}

	var status, paymentStatus, erpState string
	var checkoutID *string
	if err := testPool.QueryRow(ctx,
		`SELECT status, payment_status, erp_order_state, checkout_id FROM carts WHERE id = $1`,
		cartID).Scan(&status, &paymentStatus, &erpState, &checkoutID); err != nil {
		t.Fatalf("lendo cart restaurado: %v", err)
	}
	if status != "checkout" {
		t.Errorf("status = %q, queria \"checkout\"", status)
	}
	if paymentStatus != "paid" {
		t.Errorf("payment_status = %q, queria \"paid\"", paymentStatus)
	}
	if erpState != "none" {
		t.Errorf("erp_order_state = %q, queria \"none\"", erpState)
	}
	if checkoutID == nil || *checkoutID != "pay_123" {
		t.Errorf("checkout_id = %v, queria \"pay_123\"", checkoutID)
	}
	if _, reason := cartStatusAndReason(t, cartID); reason != "" {
		t.Errorf("cancelled_reason = %q, queria vazio", reason)
	}
	if got := productStock(t, productID); got != 0 {
		t.Errorf("estoque = %d, queria 0 (unidade retomada pela venda)", got)
	}
}

// O restore é estreito de propósito: só desfaz o cancelamento MANUAL. Um cart
// expirado por prazo não vira venda por causa de um webhook atrasado — esse
// caso continua caindo na reconciliação manual.
func TestPaymentDoesNotRestoreExpiredCart(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET status = 'expired', cancelled_reason = 'expired' WHERE id = $1`, cartID); err != nil {
		t.Fatalf("expirando cart: %v", err)
	}

	paidAt := time.Now().UTC()
	restored, err := testRepo.RestoreCancelledCartAsPaid(ctx, cartID, fx.storeID, "paid", "pay_456", &paidAt, "pix")
	if err != nil {
		t.Fatalf("RestoreCancelledCartAsPaid: %v", err)
	}
	if restored {
		t.Error("cart expirado não deveria ser restaurado como pago")
	}
}

// Mesma estreiteza para o cart morto por bloqueio do comprador: quem foi
// bloqueado não volta a comprar porque um pagamento pingou.
func TestPaymentDoesNotRestoreBlockedCart(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET status = 'cancelled', cancelled_reason = $2 WHERE id = $1`,
		cartID, CancelReasonBlocked); err != nil {
		t.Fatalf("bloqueando cart: %v", err)
	}

	paidAt := time.Now().UTC()
	restored, err := testRepo.RestoreCancelledCartAsPaid(ctx, cartID, fx.storeID, "paid", "pay_789", &paidAt, "pix")
	if err != nil {
		t.Fatalf("RestoreCancelledCartAsPaid: %v", err)
	}
	if restored {
		t.Error("cart de comprador bloqueado não deveria ser restaurado como pago")
	}
}

// seedStoreMember cria um usuário com membership ativa na loja — o público do
// aviso no sino do painel.
func seedStoreMember(t *testing.T, storeID string) string {
	t.Helper()
	ctx := context.Background()
	uniq := fmt.Sprintf("m-%d-%d", seedSeq, rand.Intn(1_000_000))
	seedSeq++
	var userID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO users (clerk_id, email, name) VALUES ('clerk-'||$1, $1||'@example.com', 'Membro')
		 RETURNING id::text`, uniq).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO memberships (store_id, user_id, role, status)
		 VALUES ($1, $2, 'owner', 'active')`, storeID, userID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	return userID
}

// O lojista precisa SABER que vendeu algo que julgava cancelado — é a única
// coisa que sobra para ele fazer (decidir se estorna por fora). O aviso vai
// para todo membro ativo da loja e não se repete quando o bus reentrega o fato.
func TestCancellationRevertedNotifiesEveryStoreMemberOnce(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)
	seedStoreMember(t, fx.storeID)
	seedStoreMember(t, fx.storeID)

	inbox := notificationinbox.NewRepository(testPool)
	payload := []byte(`{"short_id":1042,"platform_handle":"@comprador"}`)

	for i := 0; i < 2; i++ { // segunda chamada = reentrega do asynq
		if err := inbox.InsertStoreOrderNotification(ctx,
			notificationinbox.TypeOrderCancellationReverted, fx.storeID, cartID, payload); err != nil {
			t.Fatalf("InsertStoreOrderNotification (%d): %v", i+1, err)
		}
	}

	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE cart_id = $1 AND type = $2`,
		cartID, notificationinbox.TypeOrderCancellationReverted).Scan(&count); err != nil {
		t.Fatalf("contando notificações: %v", err)
	}
	if count != 2 {
		t.Errorf("notificações = %d, queria 2 (uma por membro, sem duplicar na reentrega)", count)
	}
}

// O carimbo no carrinho é o que alimenta o histórico do pedido ("cancelamento
// revertido — o comprador pagou"); sem ele o lojista não tem como entender por
// que um pedido que ele cancelou aparece pago.
func TestCancellationRevertedIsStampedOnTheCart(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 0, 0)
	cartID := seedHolderCart(t, fx, productID, 1)

	if err := scaleService().CancelCart(ctx, cartID, fx.storeID); err != nil {
		t.Fatalf("CancelCart: %v", err)
	}
	paidAt := time.Now().UTC()
	if _, err := testRepo.RestoreCancelledCartAsPaid(ctx, cartID, fx.storeID, "paid", "pay_stamp", &paidAt, "pix"); err != nil {
		t.Fatalf("RestoreCancelledCartAsPaid: %v", err)
	}

	var revertedAt *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT cancellation_reverted_at FROM carts WHERE id = $1`, cartID).Scan(&revertedAt); err != nil {
		t.Fatalf("lendo carimbo: %v", err)
	}
	if revertedAt == nil {
		t.Error("cancellation_reverted_at ficou NULL — o histórico do pedido não teria o que mostrar")
	}
}
