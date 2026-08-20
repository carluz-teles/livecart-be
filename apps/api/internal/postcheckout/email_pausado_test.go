package postcheckout_test

// O toggle da aba de Comunicações passou a valer para os E-MAILS (20/08/2026).
// Antes, `enabled=false` só desligava o texto customizado — o e-mail saía
// mesmo assim, com o template padrão: o lojista pausava e nada pausava (o
// estorno da Canto da Art chegou com o card "Pausada"). Agora pausado é
// pausado; chave ausente continua sendo "ligado", que é o default histórico.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification"
	"livecart/apps/api/internal/postcheckout"
)

// leitorDeSettings devolve settings fixas — satisfaz NotificationSettingsReader.
type leitorDeSettings struct{ s *notification.Settings }

func (l leitorDeSettings) GetSettings(_ context.Context, _ string) (*notification.Settings, error) {
	return l.s, nil
}

func servicoComSettings(t *testing.T, s *notification.Settings) (*postcheckout.Service, *fakeEmailSender) {
	t.Helper()
	fake := &fakeEmailSender{}
	svc := postcheckout.NewService(postcheckout.NewRepository(testQueries), fake, zap.NewNop())
	svc.SetNotificationService(leitorDeSettings{s})
	return svc, fake
}

func TestEmailPausadoNaoEnvia(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	svc, fake := servicoComSettings(t, &notification.Settings{
		PaymentConfirmed: &notification.EmailTemplateSettings{Enabled: false},
	})
	svc.OnCartPaid(ctx, cartID)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	if err := svc.SendPaidReceipt(ctx, cartID); err != nil {
		t.Fatalf("SendPaidReceipt: %v", err)
	}
	if len(fake.paid) != 0 {
		t.Fatalf("e-mail pausado foi enviado %d vez(es) — o toggle tem de valer", len(fake.paid))
	}
}

func TestEmailComChaveAusenteContinuaEnviando(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	cartID := seedPaidCartWithOrder(t)

	// Loja que nunca abriu a aba: settings sem NENHUMA chave de e-mail.
	svc, fake := servicoComSettings(t, &notification.Settings{})
	svc.OnCartPaid(ctx, cartID)
	setCustomerEmail(t, ctx, cartID, "buyer@example.com")

	if err := svc.SendPaidReceipt(ctx, cartID); err != nil {
		t.Fatalf("SendPaidReceipt: %v", err)
	}
	if len(fake.paid) != 1 {
		t.Fatalf("chave ausente é 'ligado' (default histórico); envios = %d", len(fake.paid))
	}
}
