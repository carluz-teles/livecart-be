package social

// Publicação de mídia com desfecho verificado.
//
// O caso que motivou tudo (19/08/2026): o media_publish de um story estourou
// o timeout MAS ENTROU — o Instagram publicou e a resposta se perdeu. O
// LiveCart devolveu 422, a lojista reenviou e nasceu um segundo story
// idêntico, sem vínculo com a sessão, engolindo respostas de compradoras.
//
// A regra destes testes é a mesma do ledger de estoque: erro na chamada não
// diz o desfecho; quem diz é o status do container. Publicar de novo às cegas
// é proibido — retry é sempre com o MESMO container.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// grafFalsa é uma Graph API de mentira com o estado de UM container.
type grafFalsa struct {
	mu sync.Mutex

	containersCriados int
	publishTentativas int
	creationIDs       []string

	// Roteiro por tentativa de publish (1-based). Valores:
	// "ok" (publica), "500" (recusa), "demora" (dorme além do timeout do
	// client MAS publica — o caso de 19/08), "demora-sem-publicar".
	publishRoteiro []string

	// Status devolvido pelo GET do container DEPOIS de alguma tentativa de
	// publish. Antes disso o container responde FINISHED (pronto). Se o
	// publish "entrou", vira PUBLISHED automaticamente.
	statusAposFalha string
	statusQuebrado  bool // GET do container responde 500 sempre

	publicado      bool
	storyID        string // id que /me/stories devolve quando publicado
	storiesLidas   int
	mediaLidas     int
	statusLeituras int
}

func (g *grafFalsa) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/me/media"):
			g.containersCriados++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("container-%d", g.containersCriados)})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/me/media_publish"):
			g.publishTentativas++
			var body struct {
				CreationID string `json:"creation_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.creationIDs = append(g.creationIDs, body.CreationID)

			acao := "ok"
			if g.publishTentativas <= len(g.publishRoteiro) {
				acao = g.publishRoteiro[g.publishTentativas-1]
			}
			switch acao {
			case "ok":
				g.publicado = true
				_ = json.NewEncoder(w).Encode(map[string]string{"id": g.storyID})
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			case "demora":
				// O caso de 19/08: o client desiste, o Instagram publica.
				g.publicado = true
				g.mu.Unlock()
				time.Sleep(400 * time.Millisecond)
				g.mu.Lock()
				_ = json.NewEncoder(w).Encode(map[string]string{"id": g.storyID})
			case "demora-sem-publicar":
				g.mu.Unlock()
				time.Sleep(400 * time.Millisecond)
				g.mu.Lock()
				w.WriteHeader(http.StatusInternalServerError)
			}

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/me/stories"):
			g.storiesLidas++
			g.listaDeMidia(w)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/me/media"):
			g.mediaLidas++
			g.listaDeMidia(w)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "container-"):
			g.statusLeituras++
			if g.statusQuebrado {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			status := "FINISHED"
			if g.publicado {
				status = "PUBLISHED"
			} else if g.publishTentativas > 0 && g.statusAposFalha != "" {
				status = g.statusAposFalha
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status_code": status})

		default:
			t.Errorf("rota inesperada na Graph falsa: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (g *grafFalsa) listaDeMidia(w http.ResponseWriter) {
	type item struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
	}
	var data []item
	if g.publicado {
		data = append(data, item{ID: g.storyID, Timestamp: time.Now().Format("2006-01-02T15:04:05-0700")})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// newPublishInstagram monta o provider contra a Graph falsa com um client de
// timeout curto — os cenários de "demora" precisam estourar em milissegundos.
func newPublishInstagram(t *testing.T, g *grafFalsa) (*Instagram, func()) {
	t.Helper()
	ig := &Instagram{
		integrationID: "int-1",
		storeID:       "store-1",
		credentials:   &providers.Credentials{AccessToken: "tok"},
		logger:        zap.NewNop(),
		client:        &http.Client{Timeout: 200 * time.Millisecond},
	}
	srv := httptest.NewServer(g.handler(t))
	originalBase := instagramGraphAPIBaseURL
	originalDelay := containerStatusRetryDelay
	instagramGraphAPIBaseURL = srv.URL
	containerStatusRetryDelay = 5 * time.Millisecond
	return ig, func() {
		instagramGraphAPIBaseURL = originalBase
		containerStatusRetryDelay = originalDelay
		srv.Close()
	}
}

func TestPublishStoryCaminhoFeliz(t *testing.T) {
	g := &grafFalsa{storyID: "story-1", publishRoteiro: []string{"ok"}}
	ig, done := newPublishInstagram(t, g)
	defer done()

	id, err := ig.PublishStory(context.Background(), "https://cdn/x.jpg", false)
	if err != nil {
		t.Fatalf("caminho feliz: %v", err)
	}
	if id != "story-1" {
		t.Fatalf("media id = %q; esperava story-1", id)
	}
	if g.storiesLidas != 0 {
		t.Errorf("caminho feliz não deveria precisar de recuperação por /me/stories")
	}
}

// O caso de 19/08, inteiro: timeout que ENTROU. Nada de segundo container —
// o status diz PUBLISHED e o media id é recuperado pela listagem de stories.
func TestPublishStoryTimeoutQueEntrouRecuperaOMedia(t *testing.T) {
	g := &grafFalsa{storyID: "story-que-entrou", publishRoteiro: []string{"demora"}}
	ig, done := newPublishInstagram(t, g)
	defer done()

	id, err := ig.PublishStory(context.Background(), "https://cdn/x.jpg", false)
	if err != nil {
		t.Fatalf("timeout que entrou deveria terminar em sucesso recuperado, veio: %v", err)
	}
	if id != "story-que-entrou" {
		t.Fatalf("media id = %q; esperava o story recuperado", id)
	}
	if g.containersCriados != 1 {
		t.Fatalf("containers criados = %d; recriar container é exatamente o bug dos 2 stories", g.containersCriados)
	}
	if g.publishTentativas != 1 {
		t.Errorf("publish tentado %d vezes; com PUBLISHED confirmado não há o que repetir", g.publishTentativas)
	}
}

// Falha comprovada (500 e container segue FINISHED): repete com o MESMO container.
func TestPublishStoryFalhaComprovadaRepeteOMesmoContainer(t *testing.T) {
	g := &grafFalsa{storyID: "story-2", publishRoteiro: []string{"500", "ok"}}
	ig, done := newPublishInstagram(t, g)
	defer done()

	id, err := ig.PublishStory(context.Background(), "https://cdn/x.jpg", false)
	if err != nil {
		t.Fatalf("falha comprovada seguida de sucesso: %v", err)
	}
	if id != "story-2" {
		t.Fatalf("media id = %q", id)
	}
	if g.containersCriados != 1 {
		t.Fatalf("containers criados = %d; o retry tem de reusar o container", g.containersCriados)
	}
	if len(g.creationIDs) != 2 || g.creationIDs[0] != g.creationIDs[1] {
		t.Fatalf("creation_ids das tentativas = %v; ambas deveriam apontar o mesmo container", g.creationIDs)
	}
}

func TestPublishStoryContainerMortoFalhaDeVez(t *testing.T) {
	g := &grafFalsa{storyID: "s", publishRoteiro: []string{"500"}, statusAposFalha: "ERROR"}
	ig, done := newPublishInstagram(t, g)
	defer done()

	_, err := ig.PublishStory(context.Background(), "https://cdn/x.jpg", false)
	if err == nil {
		t.Fatal("container ERROR tem de falhar")
	}
	if errors.Is(err, providers.ErrPublishOutcomeUnknown) {
		t.Fatalf("ERROR é desfecho CONHECIDO (não publicou); veio desconhecido: %v", err)
	}
	if g.publishTentativas != 1 {
		t.Errorf("publish tentado %d vezes num container morto", g.publishTentativas)
	}
}

// Publish falhou E o status não responde: desfecho desconhecido, com o
// container preservado no erro para o retry retomar — nunca um container novo.
func TestPublishStoryDesfechoDesconhecidoCarregaOContainer(t *testing.T) {
	g := &grafFalsa{storyID: "s", publishRoteiro: []string{"demora-sem-publicar"}}
	ig, done := newPublishInstagram(t, g)
	defer done()
	// O primeiro GET de status (waitContainerFinished) precisa funcionar para
	// chegar ao publish; só depois o status quebra.
	quebraDepoisDoPublish := func() {
		g.mu.Lock()
		g.statusQuebrado = true
		g.mu.Unlock()
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		quebraDepoisDoPublish()
	}()

	_, err := ig.PublishStory(context.Background(), "https://cdn/x.jpg", false)
	if err == nil {
		t.Fatal("esperava erro de desfecho desconhecido")
	}
	if !errors.Is(err, providers.ErrPublishOutcomeUnknown) {
		t.Fatalf("esperava ErrPublishOutcomeUnknown, veio: %v", err)
	}
	var unk *providers.PublishOutcomeUnknownError
	if !errors.As(err, &unk) || unk.ContainerID != "container-1" {
		t.Fatalf("o erro precisa carregar o container da tentativa (veio %+v) — é ele que o retry retoma", unk)
	}
	if g.containersCriados != 1 {
		t.Fatalf("containers criados = %d", g.containersCriados)
	}
}

func TestResumeContainerPublishJaPublicado(t *testing.T) {
	g := &grafFalsa{storyID: "story-orfao", publicado: true, containersCriados: 1}
	ig, done := newPublishInstagram(t, g)
	defer done()

	id, err := ig.ResumeContainerPublish(context.Background(), "container-1", true, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("resume de container PUBLISHED: %v", err)
	}
	if id != "story-orfao" {
		t.Fatalf("media id = %q; esperava recuperar o story publicado", id)
	}
	if g.publishTentativas != 0 {
		t.Errorf("resume de PUBLISHED não pode publicar de novo (tentou %d)", g.publishTentativas)
	}
}

func TestResumeContainerPublishAindaProntoPublica(t *testing.T) {
	g := &grafFalsa{storyID: "story-agora", publishRoteiro: []string{"ok"}, containersCriados: 1}
	ig, done := newPublishInstagram(t, g)
	defer done()

	id, err := ig.ResumeContainerPublish(context.Background(), "container-1", true, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("resume de container FINISHED: %v", err)
	}
	if id != "story-agora" {
		t.Fatalf("media id = %q", id)
	}
	if g.containersCriados != 1 {
		t.Fatalf("resume criou container novo (%d)", g.containersCriados)
	}
}

func TestResumeContainerPublishMortoLiberaContainerNovo(t *testing.T) {
	g := &grafFalsa{storyID: "s", statusAposFalha: "EXPIRED", publishTentativas: 1, containersCriados: 1}
	ig, done := newPublishInstagram(t, g)
	defer done()

	_, err := ig.ResumeContainerPublish(context.Background(), "container-1", true, time.Now().Add(-time.Minute))
	if !errors.Is(err, providers.ErrContainerDead) {
		t.Fatalf("container EXPIRED deveria liberar publicação nova via ErrContainerDead, veio: %v", err)
	}
}
