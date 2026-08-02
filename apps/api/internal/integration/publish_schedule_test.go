package integration

// Testes da publicação agendada (RN-31).
//
// O que precisa ficar provado, e por quê:
//
//  1. a REIVINDICAÇÃO é o que impede publicação dupla. A task asynq é
//     at-least-once e o sweep de backstop dispara pelo mesmo caminho — se dois
//     disparos conseguissem reivindicar o mesmo job, o mesmo post iria ao ar
//     duas vezes, e não há como desfazer isso no Instagram;
//  2. o CANCELAMENTO vale mesmo depois de a task ter sido entregue. O asynq
//     recusa apagar task ACTIVE, então o guard do banco é a única garantia
//     real. Um cancelamento que dependesse do Redis seria um cancelamento que
//     falha justamente na hora em que importa;
//  3. o dead-letter existe e tem teto. Sem ele um agendamento com token
//     inválido ficaria voltando para a fila para sempre;
//  4. o backstop enxerga tanto o vencido quanto o preso em 'publishing' — a
//     falha silenciosa é o pior desfecho de uma publicação prometida;
//  5. as regras de agendamento (lead, horizonte, janela) recusam ANTES de o
//     asset ser retido por dias.
//
// O que estes testes NÃO cobrem: a chamada real à Graph. Publicar exige um
// provider do Instagram e não há costura de injeção para ele neste pacote —
// registrado no relatório da fatia.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

func publishTestService() *Service {
	return &Service{repo: testRepo, logger: zap.NewNop()}
}

// seedPublishJob cria uma loja + produto e agenda uma publicação para daqui a
// uma hora, devolvendo o job.
func seedPublishJob(t *testing.T, svc *Service, kind string) *PublishJob {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var storeID, productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Loja RN31', 'rn31-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	kw := fmt.Sprintf("%d", 1000+seedSeq%9000)
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'Produto RN31', 'tiny', 'EXT-'||$2, $3, 1000, 10) RETURNING id::text`,
		storeID, n, kw,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	contentType := "image/jpeg"
	if kind == "reel" {
		contentType = "video/mp4"
	}
	job, err := svc.SchedulePublish(ctx, SchedulePublishInput{
		StoreID:          storeID,
		MediaKind:        kind,
		AssetPath:        "instagram/" + storeID + "/" + n + ".bin",
		AssetContentType: contentType,
		Title:            "Promo agendada",
		Caption:          "Chegou!",
		ProductIDs:       []string{productID},
		ScheduledFor:     time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("agendar: %v", err)
	}
	return job
}

func publishJobStatus(t *testing.T, jobID string) (status string, attempts int, lastErr string) {
	t.Helper()
	var le *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, attempts, last_error FROM session_publish_jobs WHERE id=$1::uuid`, jobID,
	).Scan(&status, &attempts, &le); err != nil {
		t.Fatalf("lendo job: %v", err)
	}
	if le != nil {
		lastErr = *le
	}
	return
}

// A reivindicação é exclusiva: o segundo disparo não acha nada.
func TestPublishJobIsClaimedOnlyOnce(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()
	job := seedPublishJob(t, svc, "post")

	first, err := testRepo.ClaimPublishJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("primeira reivindicacao: %v", err)
	}
	if first == nil {
		t.Fatal("a primeira reivindicacao devia pegar o job")
	}
	if first.Attempts != 1 {
		t.Fatalf("attempts devia virar 1, veio %d", first.Attempts)
	}

	second, err := testRepo.ClaimPublishJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("segunda reivindicacao: %v", err)
	}
	if second != nil {
		t.Fatal("a segunda reivindicacao pegou o mesmo job — o mesmo post iria ao ar duas vezes")
	}
}

// Cancelar tira o job de 'scheduled', e é isso que faz uma task já entregue
// virar no-op. Sem o guard, o Redis seria a única barreira — e ele não apaga
// task ACTIVE.
func TestCancelledJobIsNotClaimable(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()
	job := seedPublishJob(t, svc, "post")

	cancelled, err := svc.CancelPublish(ctx, job.ID, job.StoreID)
	if err != nil {
		t.Fatalf("cancelar: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("esperava status cancelled, veio %q", cancelled.Status)
	}

	claimed, err := testRepo.ClaimPublishJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("reivindicar cancelado: %v", err)
	}
	if claimed != nil {
		t.Fatal("um job CANCELADO foi reivindicado — a publicacao sairia depois de o lojista desistir")
	}

	// E o disparo, se vier assim mesmo, termina sem erro (nada a retentar).
	if err := svc.RunScheduledPublish(ctx, job.ID); err != nil {
		t.Fatalf("disparo de job cancelado devia ser no-op, veio: %v", err)
	}
}

// Cancelar o que já saiu de 'scheduled' é 422 com o motivo, não 404: "não
// existe" e "já publicou" pedem respostas diferentes na tela.
func TestCancelRefusesJobAlreadyInFlight(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()
	job := seedPublishJob(t, svc, "post")

	if _, err := testRepo.ClaimPublishJob(ctx, job.ID); err != nil {
		t.Fatalf("reivindicar: %v", err)
	}
	_, err := svc.CancelPublish(ctx, job.ID, job.StoreID)
	if err == nil {
		t.Fatal("cancelar um job em publicacao devia falhar — o container pode ja estar no ar")
	}
	if got := err.Error(); got == "" {
		t.Fatal("erro sem mensagem")
	}
}

// O teto de tentativas leva ao dead-letter em vez de voltar para a fila.
func TestPublishFailureRetriesThenDeadLetters(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()
	job := seedPublishJob(t, svc, "post")

	// Tentativa com folga: volta para 'scheduled' e devolve erro (o asynq
	// retenta).
	claimed, err := testRepo.ClaimPublishJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("reivindicar: %v", err)
	}
	if err := svc.handlePublishFailure(ctx, claimed, fmt.Errorf("graph fora do ar")); err == nil {
		t.Fatal("com tentativa sobrando o disparo tem de devolver erro, senao o asynq nao retenta")
	}
	status, attempts, lastErr := publishJobStatus(t, job.ID)
	if status != "scheduled" {
		t.Fatalf("devia voltar para a fila, veio %q", status)
	}
	if attempts != 1 || lastErr == "" {
		t.Fatalf("esperava attempts=1 com motivo gravado, veio attempts=%d last_error=%q", attempts, lastErr)
	}

	// Teto atingido: vira dead-letter e NÃO pede retry.
	claimed.Attempts = maxPublishAttempts
	if err := svc.handlePublishFailure(ctx, claimed, fmt.Errorf("token invalido")); err != nil {
		t.Fatalf("no dead-letter o disparo tem de terminar sem erro, veio: %v", err)
	}
	status, _, lastErr = publishJobStatus(t, job.ID)
	if status != "failed" {
		t.Fatalf("esperava failed, veio %q", status)
	}
	if lastErr == "" {
		t.Fatal("dead-letter sem motivo gravado — o lojista descobriria pela venda que nao aconteceu")
	}
}

// O backstop enxerga o vencido e o preso.
func TestSweepSeesDueAndStuckJobs(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()

	due := seedPublishJob(t, svc, "post")
	if _, err := testPool.Exec(ctx,
		`UPDATE session_publish_jobs SET scheduled_for = now() - interval '10 minutes' WHERE id=$1::uuid`, due.ID,
	); err != nil {
		t.Fatalf("vencer o job: %v", err)
	}
	found, err := testRepo.ListDuePublishJobs(ctx, 50)
	if err != nil {
		t.Fatalf("listar vencidos: %v", err)
	}
	if !containsJob(found, due.ID) {
		t.Fatal("o job vencido nao apareceu para o sweep — a publicacao sumiria em silencio")
	}

	stuck := seedPublishJob(t, svc, "post")
	// Um job so chega a 'publishing' na hora marcada (a task dispara em
	// scheduled_for, e o sweep so pega o que ja venceu), entao o preso REAL tem
	// scheduled_for no passado. O teste reproduz isso — sem essa parte, o
	// re-arm do liberado nao seria exercitado.
	if _, err := testPool.Exec(ctx,
		`UPDATE session_publish_jobs SET scheduled_for = now() - interval '5 minutes' WHERE id=$1::uuid`, stuck.ID,
	); err != nil {
		t.Fatalf("vencer o job preso: %v", err)
	}
	if _, err := testRepo.ClaimPublishJob(ctx, stuck.ID); err != nil {
		t.Fatalf("reivindicar: %v", err)
	}
	// Recém-reivindicado NÃO é preso: publicar um Reel demora.
	fresh, err := testRepo.ListStuckPublishJobs(ctx, time.Now().Add(-stuckPublishAfter), 50)
	if err != nil {
		t.Fatalf("listar presos: %v", err)
	}
	if containsJob(fresh, stuck.ID) {
		t.Fatal("um job recem-reivindicado foi tratado como abandonado — o sweep republicaria por cima de um publish em voo")
	}

	if _, err := testPool.Exec(ctx,
		`UPDATE session_publish_jobs SET last_attempt_at = now() - interval '1 hour' WHERE id=$1::uuid`, stuck.ID,
	); err != nil {
		t.Fatalf("envelhecer a tentativa: %v", err)
	}
	abandoned, err := testRepo.ListStuckPublishJobs(ctx, time.Now().Add(-stuckPublishAfter), 50)
	if err != nil {
		t.Fatalf("listar presos: %v", err)
	}
	if !containsJob(abandoned, stuck.ID) {
		t.Fatal("o job abandonado em 'publishing' nunca mais seria reivindicado")
	}

	// Com agendador ligado, o sweep RE-ARMA em vez de publicar aqui dentro:
	// ele roda no goroutine do ticker de 5 min e publicar um Reel bloqueia por
	// minutos. O preso volta para a fila e sai re-armado no mesmo ciclo.
	sched := &fakePublishScheduler{}
	svc.SetPublishScheduler(sched)
	svc.SweepDuePublishJobs(ctx)

	if status, _, _ := publishJobStatus(t, stuck.ID); status != "scheduled" {
		t.Fatalf("o preso devia voltar para 'scheduled', veio %q", status)
	}
	if !sched.armed(due.ID) {
		t.Fatal("o vencido nao foi re-armado — a publicacao ficaria esperando o proximo tick para sempre")
	}
	if !sched.armed(stuck.ID) {
		t.Fatal("o preso foi solto e nao re-armado — ele so sairia no ciclo seguinte")
	}
}

// fakePublishScheduler registra o que o sweep empurrou para a fila.
type fakePublishScheduler struct{ ids []string }

func (f *fakePublishScheduler) SchedulePublish(_ context.Context, jobID string, _ time.Time) error {
	f.ids = append(f.ids, jobID)
	return nil
}

func (f *fakePublishScheduler) CancelPublish(_ context.Context, _ string) error { return nil }

func (f *fakePublishScheduler) armed(jobID string) bool {
	for _, id := range f.ids {
		if id == jobID {
			return true
		}
	}
	return false
}

// As regras de agendamento recusam ANTES de o asset ficar retido por dias.
func TestSchedulePublishRules(t *testing.T) {
	requireDB(t)
	svc := publishTestService()
	ctx := context.Background()

	base := seedPublishJob(t, svc, "post")
	storeID := base.StoreID
	productID := base.ProductIDs[0]

	valid := func() SchedulePublishInput {
		return SchedulePublishInput{
			StoreID:          storeID,
			MediaKind:        "post",
			AssetPath:        "instagram/x/y.jpg",
			AssetContentType: "image/jpeg",
			ProductIDs:       []string{productID},
			ScheduledFor:     time.Now().Add(2 * time.Hour),
		}
	}
	future := time.Now().Add(3 * time.Hour)
	past := time.Now().Add(time.Hour)

	cases := []struct {
		name  string
		mut   func(*SchedulePublishInput)
		valid bool
	}{
		{"caso valido", func(*SchedulePublishInput) {}, true},
		{"sem produto", func(in *SchedulePublishInput) { in.ProductIDs = nil }, false},
		{"especie desconhecida", func(in *SchedulePublishInput) { in.MediaKind = "carousel" }, false},
		{"sem asset", func(in *SchedulePublishInput) { in.AssetPath = "" }, false},
		{"lead curto demais", func(in *SchedulePublishInput) { in.ScheduledFor = time.Now().Add(time.Minute) }, false},
		{"alem do horizonte", func(in *SchedulePublishInput) { in.ScheduledFor = time.Now().Add(90 * 24 * time.Hour) }, false},
		{"janela fecha antes de publicar", func(in *SchedulePublishInput) { in.EndsAt = &past }, false},
		{"janela coerente", func(in *SchedulePublishInput) { in.EndsAt = &future }, true},
		{"story com janela declarada", func(in *SchedulePublishInput) {
			in.MediaKind = "story"
			in.EndsAt = &future
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid()
			tc.mut(&in)
			_, err := svc.SchedulePublish(ctx, in)
			if tc.valid && err != nil {
				t.Fatalf("esperava aceitar, veio: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("esperava recusar e passou")
			}
		})
	}
}

func containsJob(jobs []PublishJob, id string) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}
