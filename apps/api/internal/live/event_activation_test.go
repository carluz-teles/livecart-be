package live

// E37 — o evento agendado nunca vendia.
//
// Nada no sistema chamava ActivateScheduledEvent: um evento criado com
// scheduled_at nascia 'scheduled' e morria 'ended' pelo sweep de ends_at, sem
// nunca passar por 'active'. O botão "Iniciar live" do painel só carimbava
// started_at na sessão (que já nasce aceitando compra), respondia 200 e não
// mudava rótulo nenhum. Do lado do comprador, todo comentário recebia "ela
// começa em <data já vencida>", para sempre.
//
// São dois caminhos de ativação e os dois têm de existir: o botão resolve o
// evento de live que o lojista acompanha; o sweep resolve post/reel/story
// publicados por agendamento, que não têm ninguém para apertar botão.

import (
	"context"
	"testing"
	"time"

	"livecart/apps/api/lib/httpx"
)

// readEventStatus devolve o status CRU do banco — não o EffectiveStatus, que é
// derivado na leitura e esconderia exatamente o que este teste mede.
func readEventStatus(t *testing.T, ctx context.Context, eventID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM live_events WHERE id = $1::uuid`, eventID,
	).Scan(&status); err != nil {
		t.Fatalf("ler status do evento: %v", err)
	}
	return status
}

// seedScheduledEvent cria um evento AGENDADO com a janela pedida, pelo caminho
// real de criação (Create é o único lugar que grava status='scheduled').
func seedScheduledEvent(t *testing.T, ctx context.Context, storeID, title string, startsIn, endsIn time.Duration) string {
	t.Helper()
	starts := time.Now().UTC().Add(startsIn).Truncate(time.Second)
	ends := time.Now().UTC().Add(endsIn).Truncate(time.Second)
	out, err := newWindowService(nil).Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: title, Type: "multi",
		ScheduledAt: &starts, EndsAt: &ends,
	})
	if err != nil {
		t.Fatalf("criar evento agendado: %v", err)
	}
	if got := readEventStatus(t, ctx, out.ID); got != "scheduled" {
		t.Fatalf("evento com scheduled_at deveria nascer 'scheduled', veio %q", got)
	}
	return out.ID
}

// O botão do lojista ATIVA o evento — não só a sessão.
func TestStartActivatesScheduledEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "act-start")
	svc := newWindowService(nil)

	eventID := seedScheduledEvent(t, ctx, storeID, "Live de segunda", -time.Hour, 24*time.Hour)

	if _, err := svc.Start(ctx, eventID, storeID); err != nil {
		t.Fatalf("iniciar evento: %v", err)
	}
	if got := readEventStatus(t, ctx, eventID); got != "active" {
		t.Fatalf("status depois do Start = %q, queria \"active\" — o botão continuaria mentindo", got)
	}

	// Idempotente: apertar de novo não é erro nem regressão de status.
	if _, err := svc.Start(ctx, eventID, storeID); err != nil {
		t.Fatalf("iniciar duas vezes: %v", err)
	}
	if got := readEventStatus(t, ctx, eventID); got != "active" {
		t.Errorf("segundo Start mudou o status para %q", got)
	}
}

// Iniciar antes da hora marcada ANTECIPA a janela. Sem isso o evento ficaria
// 'active' e mesmo assim sem vender: a regra 2 de WindowAt barra enquanto
// starts_at não chega, e o botão voltaria a mentir por outro motivo.
func TestStartPullsFutureWindowForward(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "act-early")
	svc := newWindowService(nil)

	eventID := seedScheduledEvent(t, ctx, storeID, "Live antecipada", 3*time.Hour, 24*time.Hour)

	before := time.Now().UTC()
	if _, err := svc.Start(ctx, eventID, storeID); err != nil {
		t.Fatalf("iniciar evento antes da hora: %v", err)
	}

	starts, scheduled, ends := readWindow(t, ctx, eventID)
	if starts == nil || starts.UTC().Before(before) {
		t.Fatalf("starts_at = %v, queria ter sido puxado para agora (>= %v)", starts, before)
	}
	// scheduled_at anda junto com starts_at — é a coluna que EffectiveStatus e o
	// FE ainda leem; deixá-la no futuro manteria o painel rotulando "agendado".
	if scheduled == nil || !scheduled.UTC().Equal(starts.UTC()) {
		t.Errorf("scheduled_at = %v, queria espelhar starts_at (%v)", scheduled, starts)
	}
	if ends == nil {
		t.Fatal("ends_at foi apagado ao antecipar o início")
	}
	if WindowAt(readEventStatus(t, ctx, eventID), starts, ends, time.Now().UTC()) != WindowOpen {
		t.Error("evento iniciado à mão continua fora da janela — ele não venderia")
	}
}

// Evento encerrado não volta pelo botão. Antes daqui o pedido morria adiante
// com "no active session found for event", que não explica nada ao lojista.
func TestStartRefusesEndedEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "act-ended")
	svc := newWindowService(nil)

	eventID := seedScheduledEvent(t, ctx, storeID, "Já passou", -48*time.Hour, -time.Hour)

	_, err := svc.Start(ctx, eventID, storeID)
	if err == nil {
		t.Fatal("iniciou um evento cuja janela já fechou — o sweep de ends_at o fecharia de novo em seguida")
	}
	if httpx.StatusFromError(err) != 422 {
		t.Errorf("erro de regra de negócio deveria ser 422, veio %d (%v)", httpx.StatusFromError(err), err)
	}
	if got := readEventStatus(t, ctx, eventID); got != "scheduled" {
		t.Errorf("status = %q, o Start recusado não podia ter escrito nada", got)
	}
}

// O sweep é o caminho de quem não tem botão: post/reel/story agendado.
func TestSweepActivatesEventsPastStart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "act-sweep")
	svc := newWindowService(nil)

	ready := seedScheduledEvent(t, ctx, storeID, "Hora chegou", -10*time.Minute, 24*time.Hour)
	future := seedScheduledEvent(t, ctx, storeID, "Ainda falta", 6*time.Hour, 24*time.Hour)
	// Janela inteira vencida (serviço fora do ar no fim de semana): NÃO pode ser
	// ativado. Ativá-lo custaria duas escritas, um event.ended a mais e, no
	// meio, uma janela em que ele aceita compra fora do prazo contratado.
	expired := seedScheduledEvent(t, ctx, storeID, "Perdeu a janela", -48*time.Hour, -time.Hour)

	svc.SweepScheduledEventsReadyToStart(ctx)

	if got := readEventStatus(t, ctx, ready); got != "active" {
		t.Errorf("evento com a hora vencida ficou %q — ele nunca venderia", got)
	}
	if got := readEventStatus(t, ctx, future); got != "scheduled" {
		t.Errorf("evento com início no futuro virou %q — venderia antes da hora", got)
	}
	if got := readEventStatus(t, ctx, expired); got != "scheduled" {
		t.Errorf("evento com a janela já fechada virou %q — cabe ao sweep de ends_at encerrá-lo", got)
	}

	// Idempotente: rodar de novo não reescreve nada nem quebra.
	svc.SweepScheduledEventsReadyToStart(ctx)
	if got := readEventStatus(t, ctx, ready); got != "active" {
		t.Errorf("segundo sweep mudou o status para %q", got)
	}
}
