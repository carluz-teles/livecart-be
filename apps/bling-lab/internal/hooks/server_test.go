package hooks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/livecart/bling-lab/internal/audit"
)

func bancada(t *testing.T, secret string, forward string, estrito bool) (*Server, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "webhooks"))
	if err != nil {
		t.Fatal(err)
	}
	lg, err := audit.New(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store, lg, 0, secret, forward, estrito), store
}

func entrega(t *testing.T, s *Server, corpo, assinatura string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/bling", strings.NewReader(corpo))
	if assinatura != "" {
		req.Header.Set(HeaderAssinatura, assinatura)
	}
	rec := httptest.NewRecorder()
	s.capturar(rec, req)
	return rec.Result()
}

// O evento chega, é gravado com o envelope parseado, e o Bling recebe 2xx.
// Os 2xx importam: sem eles o Bling re-entrega por 3 dias e, esgotado o retry,
// DESATIVA a configuração do webhook — cegando todas as lojas do aplicativo.
func TestEventoValidoEGravadoComEnvelope(t *testing.T) {
	s, store := bancada(t, segredoRef, "", false)

	resp := entrega(t, s, payloadRef, Assinar([]byte(payloadRef), segredoRef))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, queria 200 — o Bling desativa o webhook depois de 3 dias de falha", resp.StatusCode)
	}

	evs, err := store.Listar()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("gravou %d eventos, queria 1", len(evs))
	}
	e := evs[0]
	if e.Assinatura != Valida {
		t.Errorf("assinatura = %q, queria %q", e.Assinatura, Valida)
	}
	if e.Envelope == nil {
		t.Fatal("o envelope não foi parseado — sem ele não há companyId para rotear a loja")
	}
	if e.Envelope.Event != "stock.updated" {
		t.Errorf("event = %q", e.Envelope.Event)
	}
	if e.Envelope.CompanyID != "436c56a5679921f5f13a3d6433561773" {
		t.Errorf("companyId = %q — é por ele que a URL única resolve a loja", e.Envelope.CompanyID)
	}
	if e.Envelope.EventID != "abc-123" {
		t.Errorf("eventId = %q — é o que casa com o UNIQUE(integration_id, event_id) que já existe", e.Envelope.EventID)
	}
	// O corpo CRU tem de ser preservado byte a byte: é sobre ele que o HMAC é
	// calculado, e re-serializar o JSON quebraria a verificação.
	if e.Body != payloadRef {
		t.Error("o corpo cru não foi preservado — reserializar quebra o HMAC")
	}
}

// Modo observação: assinatura errada é REGISTRADA mas aceita. É o primeiro
// deploy — recusar por engano num evento sem replay manual é perder o evento.
func TestModoObservacaoAceitaAssinaturaInvalida(t *testing.T) {
	s, store := bancada(t, segredoRef, "", false)

	resp := entrega(t, s, payloadRef, Assinar([]byte(payloadRef), "segredo-errado"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, queria 200 no modo observação", resp.StatusCode)
	}
	evs, _ := store.Listar()
	if len(evs) != 1 || evs[0].Assinatura != Divergente {
		t.Fatalf("queria 1 evento com assinatura %q, veio %+v", Divergente, evs)
	}
}

// Modo estrito: só depois de a observação provar que a assinatura bate sempre.
func TestModoEstritoRecusa(t *testing.T) {
	s, store := bancada(t, segredoRef, "", true)

	for _, c := range []struct {
		nome       string
		assinatura string
	}{
		{"segredo errado", Assinar([]byte(payloadRef), "outro")},
		{"sem header", ""},
		{"malformada", "sha256=zzz"},
	} {
		t.Run(c.nome, func(t *testing.T) {
			resp := entrega(t, s, payloadRef, c.assinatura)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status %d, queria 401", resp.StatusCode)
			}
		})
	}

	// Mesmo recusando, o evento é GRAVADO — é a prova de que alguém tentou.
	evs, _ := store.Listar()
	if len(evs) != 3 {
		t.Errorf("gravou %d eventos, queria 3 — recusar não pode significar esquecer", len(evs))
	}
}

// A aplicação estar fora NÃO pode virar 5xx para o Bling: ele re-entregaria por
// três dias e no fim desativaria a config, e o payload já está gravado aqui.
func TestAppForaNaoViraErroParaOBling(t *testing.T) {
	s, store := bancada(t, segredoRef, "http://127.0.0.1:1/nao-existe", false)

	resp := entrega(t, s, payloadRef, Assinar([]byte(payloadRef), segredoRef))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, queria 200 mesmo com a aplicação fora", resp.StatusCode)
	}
	corpo, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(corpo), `"forwarded":false`) {
		t.Errorf("a resposta devia dizer que não encaminhou: %s", corpo)
	}
	if evs, _ := store.Listar(); len(evs) != 1 {
		t.Error("o evento tem de ficar gravado mesmo sem encaminhar")
	}
}

// Encaminhamento: a assinatura ORIGINAL do Bling tem de chegar à aplicação,
// senão ela não consegue validar o que recebeu.
func TestEncaminhaPreservandoCorpoEAssinatura(t *testing.T) {
	var vistoCorpo, vistaAssinatura string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		vistoCorpo = string(b)
		vistaAssinatura = r.Header.Get(HeaderAssinatura)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer app.Close()

	s, _ := bancada(t, segredoRef, app.URL, false)
	assinatura := Assinar([]byte(payloadRef), segredoRef)

	resp := entrega(t, s, payloadRef, assinatura)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status %d — a resposta da aplicação tem de ser espelhada para o Bling", resp.StatusCode)
	}
	if vistoCorpo != payloadRef {
		t.Error("o corpo chegou alterado na aplicação — o HMAC dela não fecharia")
	}
	if vistaAssinatura != assinatura {
		t.Errorf("a assinatura não foi repassada: %q", vistaAssinatura)
	}
}

// Corpo que não é o envelope esperado não pode derrubar o receptor: durante a
// descoberta, o pior resultado possível é perder a entrega.
func TestCorpoInesperadoAindaEGravado(t *testing.T) {
	s, store := bancada(t, segredoRef, "", false)

	resp := entrega(t, s, `nao e json`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, queria 200", resp.StatusCode)
	}
	evs, _ := store.Listar()
	if len(evs) != 1 {
		t.Fatalf("gravou %d, queria 1", len(evs))
	}
	if evs[0].Envelope != nil {
		t.Error("não devia ter parseado envelope de um corpo que não é JSON")
	}
	if !strings.Contains(evs[0].Resumo(), "não é o envelope esperado") {
		t.Errorf("o resumo devia sinalizar o corpo inesperado: %q", evs[0].Resumo())
	}
}

// Reenviar é como se testa idempotência sem depender do Bling entregar de novo:
// o mesmo corpo, re-assinado, N vezes.
func TestReenviarReassinaComOSegredoLocal(t *testing.T) {
	var recebidas int
	var ultimaAssinatura string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if Verificar(b, r.Header.Get(HeaderAssinatura), segredoRef) == Valida {
			recebidas++
		}
		ultimaAssinatura = r.Header.Get(HeaderAssinatura)
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()

	ev := &Evento{Body: payloadRef, Headers: http.Header{}}
	if err := Reenviar(context.Background(), ev, app.URL, segredoRef, 3); err != nil {
		t.Fatal(err)
	}
	if recebidas != 3 {
		t.Fatalf("%d entregas com assinatura válida, queria 3 (última: %q)", recebidas, ultimaAssinatura)
	}
}
