package integration

// O incidente dos 2 stories (19/08/2026), reproduzido contra o Postgres real.
//
// Sequência de produção: media_publish estourou timeout MAS ENTROU (story
// publicado, resposta perdida) → 422 para a lojista → reenvio 3s depois → o
// Start de idempotência colidiu no unique e o código publicou assim mesmo →
// segundo story idêntico, sem vínculo com a sessão, respostas de compradoras
// caindo em "no matching story session".
//
// Estes testes provam a semântica nova de posse (Claim) na altura exata do
// bug — publishWithIdempotency — com o registro de idempotência de verdade:
// desfecho desconhecido fica RETOMÁVEL (o retry recebe o container antigo),
// tentativa em voo vira 409, sucesso anterior vira replay, e mídia publicada
// sem vínculo é reusada em vez de repostada.
//
// Pré-requisito local:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' go test ./apps/api/internal/integration/ -run StoryDuplicado -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
)

// seedLojaComInstagram cria loja + integração social ativa (o FK do registro
// de idempotência exige a integração).
func seedLojaComInstagram(t *testing.T) string {
	t.Helper()
	seedSeq++
	n := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000, seedSeq)

	var storeID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('Loja Story', 'story-'||$1) RETURNING id::text`, n).
		Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO integrations (store_id, type, provider, status)
		 VALUES ($1, 'social', 'instagram', 'active')`, storeID); err != nil {
		t.Fatalf("seed integração instagram: %v", err)
	}
	return storeID
}

func servicoDePublicacao() *Service {
	return &Service{
		repo:        testRepo,
		idempotency: idempotency.NewService(NewIdempotencyRepository(sqlc.New(testPool))),
		logger:      zap.NewNop(),
	}
}

func registroDeIdempotencia(t *testing.T, storeID, key string) (status string, payload []byte) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, COALESCE(response_payload, '{}'::jsonb)
		 FROM idempotency_keys WHERE store_id = $1 AND idempotency_key = $2`,
		storeID, key).Scan(&status, &payload); err != nil {
		t.Fatalf("lendo registro de idempotência: %v", err)
	}
	return status, payload
}

// O incidente inteiro: desfecho desconhecido → retry RETOMA o container →
// terceira tentativa recebe o replay do sucesso.
func TestStoryDuplicadoTimeoutQueEntrouViraRetomadaENaoDuplica(t *testing.T) {
	requireDB(t)
	storeID := seedLojaComInstagram(t)
	s := servicoDePublicacao()
	input := CreateInstagramPostInput{StoreID: storeID, Title: "Story da live", IdempotencyKey: "story-19ago"}

	// Tentativa 1: o timeout que ENTROU. O publisher devolve o erro tipado
	// com o container, como publishInstagramStoryEvent faz.
	falhaDesconhecida := errors.Join(
		httpx.ErrUnprocessable("Instagram did not confirm the publish"),
		&providers.PublishOutcomeUnknownError{ContainerID: "container-19ago", Err: errors.New("context deadline exceeded")},
	)
	_, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			if prior.ContainerID != "" {
				t.Fatalf("primeira tentativa não tem container anterior (veio %q)", prior.ContainerID)
			}
			return live.CreateLiveOutput{}, falhaDesconhecida
		})
	if err == nil {
		t.Fatal("tentativa 1 tinha de falhar")
	}
	status, payload := registroDeIdempotencia(t, storeID, "story-19ago")
	if status != "failed" {
		t.Fatalf("registro após desfecho desconhecido = %q; esperava failed", status)
	}
	var meta struct {
		Outcome     string `json:"outcome"`
		ContainerID string `json:"container_id"`
	}
	if json.Unmarshal(payload, &meta) != nil || meta.Outcome != "unknown" || meta.ContainerID != "container-19ago" {
		t.Fatalf("payload da falha precisa apontar o container para a retomada; veio %s", payload)
	}

	// Tentativa 2: o reenvio da lojista. Em 19/08 isto publicou o segundo
	// story; agora tem de chegar com o container antigo para retomar.
	out, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			if prior.ContainerID != "container-19ago" {
				t.Fatalf("retry recebeu prior %+v; sem o container ele publicaria um story novo", prior)
			}
			return live.CreateLiveOutput{ID: "ev-1"}, nil
		})
	if err != nil || out.ID != "ev-1" {
		t.Fatalf("retry com retomada: out=%+v err=%v", out, err)
	}

	// Tentativa 3: replay do sucesso, sem publicar nada.
	out, err = s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(priorPublishAttempt) (live.CreateLiveOutput, error) {
			t.Fatal("terceira tentativa não pode publicar — a resposta já existe")
			return live.CreateLiveOutput{}, nil
		})
	if err != nil || out.ID != "ev-1" {
		t.Fatalf("replay: out=%+v err=%v", out, err)
	}
}

// A corrida de 3 segundos: tentativa 1 ainda em voo quando a 2 chega.
// Antes: warn + segundo publish. Agora: 409, sem tocar no Instagram.
func TestStoryDuplicadoSegundaTentativaComPrimeiraEmVooRecebe409(t *testing.T) {
	requireDB(t)
	storeID := seedLojaComInstagram(t)
	s := servicoDePublicacao()
	input := CreateInstagramPostInput{StoreID: storeID, Title: "Story da live", IdempotencyKey: "story-em-voo"}

	// Tentativa 1 em voo DE VERDADE: parada dentro do publish, como o
	// media_publish lento de 19/08.
	emVoo := make(chan struct{})
	libera := make(chan struct{})
	terminou := make(chan struct{})
	go func() {
		defer close(terminou)
		_, _ = s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
			func(priorPublishAttempt) (live.CreateLiveOutput, error) {
				close(emVoo)
				<-libera
				return live.CreateLiveOutput{ID: "ev-lento"}, nil
			})
	}()
	<-emVoo

	_, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(priorPublishAttempt) (live.CreateLiveOutput, error) {
			t.Error("com a primeira em voo, a segunda NÃO pode publicar — era o bug dos 2 stories")
			return live.CreateLiveOutput{}, nil
		})
	close(libera)
	<-terminou
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 409 {
		t.Fatalf("esperava 409 de publicação em andamento, veio: %v", err)
	}
}

// Publicou e falhou só no vínculo com o evento: o retry reusa a mídia.
func TestStoryDuplicadoPublicadoSemVinculoReusaAMidia(t *testing.T) {
	requireDB(t)
	storeID := seedLojaComInstagram(t)
	s := servicoDePublicacao()
	input := CreateInstagramPostInput{StoreID: storeID, Title: "Story da live", IdempotencyKey: "story-sem-vinculo"}

	_, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(priorPublishAttempt) (live.CreateLiveOutput, error) {
			return live.CreateLiveOutput{}, &publishedUnboundError{MediaID: "media-77", Err: httpx.ErrInternal("bind falhou")}
		})
	if err == nil {
		t.Fatal("tentativa 1 tinha de falhar")
	}

	out, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			if prior.MediaID != "media-77" {
				t.Fatalf("retry recebeu prior %+v; sem a mídia ele publicaria um story igual", prior)
			}
			return live.CreateLiveOutput{ID: "ev-2"}, nil
		})
	if err != nil || out.ID != "ev-2" {
		t.Fatalf("retry reusando mídia: out=%+v err=%v", out, err)
	}
}

// Chave nova é pedido novo: nada de trava nem retomada entre chaves distintas.
func TestStoryDuplicadoChaveNovaPublicaDoZero(t *testing.T) {
	requireDB(t)
	storeID := seedLojaComInstagram(t)
	s := servicoDePublicacao()

	for i, key := range []string{"story-a", "story-b"} {
		input := CreateInstagramPostInput{StoreID: storeID, Title: "Story da live", IdempotencyKey: key}
		out, err := s.publishWithIdempotency(context.Background(), input, "create_instagram_story", nil,
			func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
				if prior.ContainerID != "" || prior.MediaID != "" {
					t.Fatalf("chave nova não herda tentativa anterior: %+v", prior)
				}
				return live.CreateLiveOutput{ID: fmt.Sprintf("ev-%d", i)}, nil
			})
		if err != nil || out.ID != fmt.Sprintf("ev-%d", i) {
			t.Fatalf("publicação %s: out=%+v err=%v", key, out, err)
		}
	}
}
