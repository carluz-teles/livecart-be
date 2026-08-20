package integration

// Edição do prazo do carrinho DEPOIS do evento criado (pedido do cliente,
// 20/08/2026: o teto de 24h virou 30 dias e mudar no evento tem de valer para
// quem já está com o relógio correndo).
//
// A regra sob teste é a do live.Service.Update → applyCartExpirationChange:
// grava o override e propaga o DELTA do prazo EFETIVO para os carrinhos
// abertos — deslocando, nunca recalculando. Pago, terminal e RN-04 (sem
// relógio) ficam intocados; close_cart_on_event_end desligado zera o delta
// porque o efetivo é o prazo estendido.
//
// Roda no harness do pacote (Postgres real, migrations completas) porque a
// regra É o SQL: o WHERE do ShiftOpenCartExpirations e o COALESCE do
// GetEventCartSettings não têm como ser provados num fake.
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/postgres?sslmode=disable' go test ./apps/api/internal/integration/ -run PrazoDoEvento -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/live"
)

type reschedulerEspiao struct{ movidos []string }

func (r *reschedulerEspiao) RescheduleExpiry(_ context.Context, cartID string) error {
	r.movidos = append(r.movidos, cartID)
	return nil
}

type prazoFixture struct {
	storeID, eventID       string
	aberto, pago, expirado string
	svc                    *live.Service
	espiao                 *reschedulerEspiao
}

// seedEventoEncerrado monta loja (prazo default 60) + evento encerrado com
// override de prazo + três carrinhos: um aberto com relógio correndo, um pago
// e um expirado.
func seedEventoEncerrado(t *testing.T, closeCartOnEnd bool, override *int) prazoFixture {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var fx prazoFixture
	mustScan := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("seed %q: %v", sql[:min(60, len(sql))], err)
		}
	}

	mustScan(&fx.storeID,
		`INSERT INTO stores (name, slug, cart_expiration_minutes) VALUES ('Loja Prazo', 'prazo-'||$1, 60) RETURNING id::text`, n)
	mustScan(&fx.eventID,
		`INSERT INTO live_events (store_id, status, title, ends_at, close_cart_on_event_end, cart_expiration_minutes)
		 VALUES ($1, 'ended', 'Live Prazo', now() - interval '1 hour', $2, $3) RETURNING id::text`,
		fx.storeID, closeCartOnEnd, override)

	cart := func(dst *string, suffix, status, payment string, expires any) {
		mustScan(dst,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, expires_at)
			 VALUES ($1, 'u-'||$2, '@b'||$2, 'tok-'||$2, ($3)::bigint % 100000, $4, $5, $6) RETURNING id::text`,
			fx.eventID, n+suffix, n, status, payment, expires)
	}
	// Aberto: encerrou há 1h com prazo de 60min → vence "agora". É exatamente o
	// carrinho que o cliente quer resgatar ao aumentar o prazo.
	cart(&fx.aberto, "a", "checkout", "pending", time.Now().UTC())
	cart(&fx.pago, "p", "checkout", "paid", time.Now().UTC().Add(30*time.Minute))
	cart(&fx.expirado, "e", "expired", "pending", time.Now().UTC().Add(-30*time.Minute))

	fx.espiao = &reschedulerEspiao{}
	fx.svc = live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	fx.svc.SetCartExpiryRescheduler(fx.espiao)
	return fx
}

func expiraEm(t *testing.T, cartID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT expires_at FROM carts WHERE id = $1`, cartID).Scan(&at); err != nil {
		t.Fatalf("lendo expires_at: %v", err)
	}
	return at
}

func atualizaPrazo(t *testing.T, fx prazoFixture, minutos int) {
	t.Helper()
	if _, err := fx.svc.Update(context.Background(), live.UpdateLiveInput{
		ID: fx.eventID, StoreID: fx.storeID, Title: "Live Prazo",
		CartExpirationMinutes: &minutos,
	}); err != nil {
		t.Fatalf("Update com prazo novo: %v", err)
	}
}

// O caso do cliente: evento encerrado com prazo de 60min, lojista sobe para 3
// dias. O carrinho aberto ganha as ~71h de diferença; pago e expirado não se
// movem; só o aberto tem a task cart.expire movida.
func TestPrazoDoEventoAumentoPropagaParaOsAbertos(t *testing.T) {
	requireDB(t)
	override := 60
	fx := seedEventoEncerrado(t, true, &override)
	antes := expiraEm(t, fx.aberto)
	pagoAntes := expiraEm(t, fx.pago)
	expiradoAntes := expiraEm(t, fx.expirado)

	atualizaPrazo(t, fx, 3*1440) // 3 dias

	delta := expiraEm(t, fx.aberto).Sub(*antes)
	if want := time.Duration(3*1440-60) * time.Minute; delta != want {
		t.Errorf("carrinho aberto deslocou %v; esperava %v (delta do prazo efetivo)", delta, want)
	}
	if !expiraEm(t, fx.pago).Equal(*pagoAntes) {
		t.Errorf("carrinho PAGO se moveu — pagamento neutraliza o prazo (A10)")
	}
	if !expiraEm(t, fx.expirado).Equal(*expiradoAntes) {
		t.Errorf("carrinho EXPIRADO se moveu — edição de configuração não ressuscita desfecho")
	}
	if len(fx.espiao.movidos) != 1 || fx.espiao.movidos[0] != fx.aberto {
		t.Errorf("cart.expire movido para %v; esperava exatamente o carrinho aberto", fx.espiao.movidos)
	}

	var gravado int
	if err := testPool.QueryRow(context.Background(),
		`SELECT cart_expiration_minutes FROM live_events WHERE id = $1`, fx.eventID).Scan(&gravado); err != nil {
		t.Fatalf("lendo override: %v", err)
	}
	if gravado != 3*1440 {
		t.Errorf("override gravado = %d; esperava %d", gravado, 3*1440)
	}
}

// Encurtar também propaga — inclusive para o passado: o cart.expire re-armado
// dispara na hora e o guard decide, como em qualquer vencimento.
func TestPrazoDoEventoEncurtarPuxaAJanelaParaTras(t *testing.T) {
	requireDB(t)
	override := 1440
	fx := seedEventoEncerrado(t, true, &override)
	antes := expiraEm(t, fx.aberto)

	atualizaPrazo(t, fx, 60)

	delta := expiraEm(t, fx.aberto).Sub(*antes)
	if want := -time.Duration(1440-60) * time.Minute; delta != want {
		t.Errorf("deslocou %v; esperava %v", delta, want)
	}
}

// Mesmo valor → delta zero → nada se move e nenhuma task é tocada.
func TestPrazoDoEventoMesmoValorNaoMexeEmNada(t *testing.T) {
	requireDB(t)
	override := 60
	fx := seedEventoEncerrado(t, true, &override)
	antes := expiraEm(t, fx.aberto)

	atualizaPrazo(t, fx, 60)

	if !expiraEm(t, fx.aberto).Equal(*antes) {
		t.Errorf("delta zero moveu carrinho")
	}
	if len(fx.espiao.movidos) != 0 {
		t.Errorf("delta zero re-armou task: %v", fx.espiao.movidos)
	}
}

// close_cart_on_event_end desligado: o relógio dos carrinhos é do prazo
// ESTENDIDO — mudar o prazo curto grava a coluna mas não desloca ninguém.
func TestPrazoDoEventoToggleDesligadoNaoDeslocaCarrinho(t *testing.T) {
	requireDB(t)
	override := 60
	fx := seedEventoEncerrado(t, false, &override)
	antes := expiraEm(t, fx.aberto)

	atualizaPrazo(t, fx, 3*1440)

	if !expiraEm(t, fx.aberto).Equal(*antes) {
		t.Errorf("prazo curto não rege este evento e mesmo assim o carrinho se moveu")
	}
	var gravado int
	_ = testPool.QueryRow(context.Background(),
		`SELECT cart_expiration_minutes FROM live_events WHERE id = $1`, fx.eventID).Scan(&gravado)
	if gravado != 3*1440 {
		t.Errorf("a coluna tem de ser gravada mesmo sem propagação (vale no próximo fechamento)")
	}
}

// Evento que herdava da loja (override NULL): o delta parte do valor efetivo
// herdado (60 da loja), não de zero.
func TestPrazoDoEventoHerdadoDeltaParteDaLoja(t *testing.T) {
	requireDB(t)
	fx := seedEventoEncerrado(t, true, nil)
	antes := expiraEm(t, fx.aberto)

	atualizaPrazo(t, fx, 4320) // 3 dias em horas quebradas: 72h

	delta := expiraEm(t, fx.aberto).Sub(*antes)
	if want := time.Duration(4320-60) * time.Minute; delta != want {
		t.Errorf("deslocou %v; esperava %v (efetivo herdado era 60)", delta, want)
	}
}

// RN-04: evento ATIVO não tem relógio (expires_at NULL) — editar o prazo não
// inventa um; o valor novo vale sozinho no fechamento.
func TestPrazoDoEventoAtivoNaoInventaRelogio(t *testing.T) {
	requireDB(t)
	override := 60
	fx := seedEventoEncerrado(t, true, &override)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE live_events SET status = 'active', ends_at = now() + interval '2 hours' WHERE id = $1`,
		fx.eventID); err != nil {
		t.Fatalf("reativando evento: %v", err)
	}
	var ativo string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, expires_at)
		 VALUES ($1, 'u-rn04', '@rn04', 'tok-rn04-unico', 4, 'active', 'pending', NULL) RETURNING id::text`,
		fx.eventID).Scan(&ativo); err != nil {
		t.Fatalf("seed carrinho ativo: %v", err)
	}

	atualizaPrazo(t, fx, 2880)

	if got := expiraEm(t, ativo); got != nil {
		t.Errorf("carrinho sob RN-04 ganhou expires_at %v na edição de prazo", got)
	}
}
