package idempotency

// Semântica de posse do Claim.
//
// O antecessor (Start) tratava violação de unique como "não consegui
// registrar" e o chamador publicava assim mesmo — foi assim que nasceram os
// 2 stories de 19/08/2026. Estes testes provam cada desfecho de posse.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type repoFake struct {
	porChave map[string]*Record // storeID+"|"+key
	seq      int

	criarErr    error
	reclaimHook func(id string) (bool, error)
}

func newRepoFake() *repoFake {
	return &repoFake{porChave: map[string]*Record{}}
}

func (r *repoFake) chave(storeID, key string) string { return storeID + "|" + key }

func (r *repoFake) GetByKey(_ context.Context, storeID, key string) (*Record, error) {
	rec, ok := r.porChave[r.chave(storeID, key)]
	if !ok {
		return nil, nil
	}
	c := *rec
	return &c, nil
}

func (r *repoFake) GetByHash(context.Context, string, string, time.Time) (*Record, error) {
	return nil, nil
}

func (r *repoFake) Create(_ context.Context, p CreateParams) (*Record, error) {
	if r.criarErr != nil {
		return nil, r.criarErr
	}
	k := r.chave(p.StoreID, p.IdempotencyKey)
	if _, existe := r.porChave[k]; existe {
		return nil, fmt.Errorf("duplicate key value violates unique constraint: %w", ErrDuplicateKey)
	}
	r.seq++
	rec := &Record{
		ID:             fmt.Sprintf("rec-%d", r.seq),
		IdempotencyKey: p.IdempotencyKey,
		StoreID:        p.StoreID,
		Operation:      p.Operation,
		RequestHash:    p.RequestHash,
		Status:         "pending",
		CreatedAt:      time.Now(),
	}
	r.porChave[k] = rec
	c := *rec
	return &c, nil
}

func (r *repoFake) Update(_ context.Context, id string, response []byte, status string) error {
	for _, rec := range r.porChave {
		if rec.ID == id {
			rec.Response = response
			rec.Status = status
			return nil
		}
	}
	return fmt.Errorf("record %s not found", id)
}

func (r *repoFake) Reclaim(_ context.Context, id string) (bool, error) {
	if r.reclaimHook != nil {
		return r.reclaimHook(id)
	}
	for _, rec := range r.porChave {
		if rec.ID == id {
			velhoDemais := rec.Status == "pending" && time.Since(rec.CreatedAt) > stalePendingAfter
			if rec.Status == "failed" || velhoDemais {
				rec.Status = "pending"
				rec.Response = nil
				return true, nil
			}
			return false, nil
		}
	}
	return false, nil
}

func reqDeTeste(key string) CheckRequest {
	return CheckRequest{
		IdempotencyKey: key,
		StoreID:        "store-1",
		IntegrationID:  "int-1",
		Operation:      "create_instagram_story",
		Payload:        map[string]string{"title": "Story da live"},
	}
}

func TestClaimNovoRegistraPendente(t *testing.T) {
	svc := NewService(newRepoFake())

	got, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.Record == nil || got.Record.Status != "pending" {
		t.Fatalf("esperava registro pendente próprio, veio %+v", got)
	}
	if got.Reclaimed || got.Completed != nil || got.Unguarded {
		t.Fatalf("primeiro Claim não pode ser retomada/dedupe: %+v", got)
	}
}

func TestClaimDeCompletadoDevolveARespostaOriginal(t *testing.T) {
	repo := newRepoFake()
	svc := NewService(repo)

	primeiro, _ := svc.Claim(context.Background(), reqDeTeste("k1"))
	if err := svc.Complete(context.Background(), primeiro.Record.ID, map[string]string{"event_id": "ev-1"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	segundo, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if err != nil {
		t.Fatalf("Claim repetido de operação completa: %v", err)
	}
	if segundo.Completed == nil || len(segundo.Completed.Response) == 0 {
		t.Fatalf("esperava a resposta original, veio %+v", segundo)
	}
}

// O coração do bug de 19/08: tentativa 1 ainda rodando, tentativa 2 chega.
// Antes: warn + publica de novo. Agora: ErrInFlight, sem execução dupla.
func TestClaimComPendenteFrescoRecusaEmVoo(t *testing.T) {
	svc := NewService(newRepoFake())

	if _, err := svc.Claim(context.Background(), reqDeTeste("k1")); err != nil {
		t.Fatalf("primeiro Claim: %v", err)
	}
	_, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("segundo Claim com o primeiro em voo tem de ser ErrInFlight, veio: %v", err)
	}
}

func TestClaimDeFalhadoTomaPosseEEntregaOPayloadAnterior(t *testing.T) {
	svc := NewService(newRepoFake())

	primeiro, _ := svc.Claim(context.Background(), reqDeTeste("k1"))
	err := svc.FailWithMeta(context.Background(), primeiro.Record.ID,
		errors.New("timeout"), map[string]any{"outcome": "unknown", "container_id": "container-1"})
	if err != nil {
		t.Fatalf("FailWithMeta: %v", err)
	}

	segundo, cErr := svc.Claim(context.Background(), reqDeTeste("k1"))
	if cErr != nil {
		t.Fatalf("Claim de registro falhado: %v", cErr)
	}
	if !segundo.Reclaimed || segundo.Record == nil {
		t.Fatalf("esperava retomada, veio %+v", segundo)
	}
	if want := `"container_id":"container-1"`; !contem(segundo.PriorResponse, want) {
		t.Fatalf("PriorResponse precisa carregar o container da tentativa anterior; veio %s", segundo.PriorResponse)
	}
}

func TestClaimDePendenteVelhoTomaPosse(t *testing.T) {
	repo := newRepoFake()
	svc := NewService(repo)

	primeiro, _ := svc.Claim(context.Background(), reqDeTeste("k1"))
	// A tentativa dona morreu há muito: envelhece o registro além da janela.
	repo.porChave[repo.chave("store-1", "k1")].CreatedAt = time.Now().Add(-10 * time.Minute)
	_ = primeiro

	segundo, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if err != nil {
		t.Fatalf("Claim de pendente velho: %v", err)
	}
	if !segundo.Reclaimed {
		t.Fatalf("pendente velho é tentativa morta; esperava retomada, veio %+v", segundo)
	}
}

// Corrida na retomada: dois retries simultâneos de uma falha — só um ganha o CAS.
func TestClaimComCASPerdidoRecusaEmVoo(t *testing.T) {
	repo := newRepoFake()
	svc := NewService(repo)

	primeiro, _ := svc.Claim(context.Background(), reqDeTeste("k1"))
	_ = svc.Fail(context.Background(), primeiro.Record.ID, errors.New("boom"))
	repo.reclaimHook = func(string) (bool, error) { return false, nil }

	_, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("CAS perdido tem de virar ErrInFlight, veio: %v", err)
	}
}

func TestClaimComChaveReusadaEmConteudoDiferente(t *testing.T) {
	svc := NewService(newRepoFake())

	if _, err := svc.Claim(context.Background(), reqDeTeste("k1")); err != nil {
		t.Fatalf("primeiro Claim: %v", err)
	}
	outra := reqDeTeste("k1")
	outra.Payload = map[string]string{"title": "OUTRO story"}
	_, err := svc.Claim(context.Background(), outra)
	if !errors.Is(err, ErrPayloadMismatch) {
		t.Fatalf("chave reusada com payload diferente é bug do cliente, veio: %v", err)
	}
}

// Colisão estrutural da chave vazia: registros sem chave explícita de
// OPERAÇÕES diferentes batem no mesmo unique — aí não há o que travar e o
// chamador segue sem registro (comportamento antigo), nunca um 409 falso.
func TestClaimComChaveVaziaColidindoSegueSemTrava(t *testing.T) {
	svc := NewService(newRepoFake())

	primeira := reqDeTeste("")
	if _, err := svc.Claim(context.Background(), primeira); err != nil {
		t.Fatalf("primeiro Claim sem chave: %v", err)
	}
	segunda := reqDeTeste("")
	segunda.Operation = "create_instagram_post"
	segunda.Payload = map[string]string{"caption": "outro conteúdo"}

	got, err := svc.Claim(context.Background(), segunda)
	if err != nil {
		t.Fatalf("colisão de chave vazia não pode virar erro: %v", err)
	}
	if !got.Unguarded {
		t.Fatalf("esperava seguir sem trava, veio %+v", got)
	}
}

func TestClaimComErroDeInfraPropaga(t *testing.T) {
	repo := newRepoFake()
	repo.criarErr = errors.New("db down")
	svc := NewService(repo)

	_, err := svc.Claim(context.Background(), reqDeTeste("k1"))
	if err == nil || errors.Is(err, ErrInFlight) {
		t.Fatalf("erro de infra propaga para o chamador decidir (publicar sem trava), veio: %v", err)
	}
}

func contem(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
