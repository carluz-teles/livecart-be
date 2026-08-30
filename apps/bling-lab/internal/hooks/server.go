package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
)

type Server struct {
	store   *Store
	audit   *audit.Log
	port    int
	secret  string
	forward string
	seq     atomic.Int64

	// recusar liga o modo estrito: assinatura inválida vira 401. Default FALSE
	// — o primeiro deploy só OBSERVA, porque recusar por engano num webhook que
	// não tem replay manual é perder o evento para sempre. O Bling ao menos
	// re-entrega por 3 dias, mas só se o erro for transitório do nosso lado.
	recusar bool
}

func NewServer(store *Store, lg *audit.Log, port int, secret, forward string, recusar bool) *Server {
	return &Server{store: store, audit: lg, port: port, secret: secret, forward: forward, recusar: recusar}
}

// Serve sobe o receptor. Catch-all de propósito: durante a descoberta, um 404
// nosso por divergência de caminho é pior do que aceitar tudo e registrar onde
// chegou.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", s.capturar)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()

	fmt.Printf("ponte de webhooks ouvindo em http://localhost:%d\n", s.port)
	fmt.Printf("gravando em %s\n", s.store.Dir())
	switch {
	case s.secret == "":
		fmt.Println("⚠ SEM client_secret — a assinatura será registrada como 'unconfigured' e NÃO verificada.")
	case s.recusar:
		fmt.Println("assinatura: modo ESTRITO (assinatura inválida devolve 401).")
	default:
		fmt.Println("assinatura: modo OBSERVAÇÃO (verifica e registra, mas aceita tudo).")
	}
	if s.forward != "" {
		fmt.Printf("encaminhando para %s\n", s.forward)
	}
	fmt.Println()
	fmt.Println("O Bling exige resposta 2xx em ATÉ 5 SEGUNDOS; depois disso re-entrega por 3 dias")
	fmt.Println("e, esgotado o retry, DESATIVA a configuração do webhook na UI do aplicativo.")
	fmt.Println()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) capturar(w http.ResponseWriter, r *http.Request) {
	corpo, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "erro lendo corpo", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	agora := time.Now().UTC()
	desfecho := Verificar(corpo, r.Header.Get(HeaderAssinatura), s.secret)

	ev := &Evento{
		ID:         fmt.Sprintf("%s-%03d", agora.Format("20060102T150405.000"), s.seq.Add(1)),
		ReceivedAt: agora,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Headers:    r.Header.Clone(),
		Body:       string(corpo),
		RemoteAddr: r.RemoteAddr,
		Assinatura: desfecho,
	}
	var env Envelope
	if json.Unmarshal(corpo, &env) == nil && env.Event != "" {
		ev.Envelope = &env
	}

	if err := s.store.Salvar(ev); err != nil {
		fmt.Printf("  ✗ FALHA AO GRAVAR o webhook %s: %v\n", ev.ID, err)
	}
	_ = s.audit.Append(audit.Entry{
		Kind: "webhook", Method: ev.Method, URL: ev.Path,
		Headers: audit.Headers(r.Header), ResponseRaw: ev.Resumo(),
		Note: ev.ID + " assinatura=" + string(desfecho),
	})

	marca := map[Desfecho]string{
		Valida: "✓", Divergente: "✗", Ausente: "?", Malformada: "?", SemSegredo: "—",
	}[desfecho]
	fmt.Printf("← %s %s %s %s  [assinatura %s]\n   %s\n",
		agora.Format("15:04:05.000"), marca, ev.Method, ev.Path, desfecho, ev.Resumo())

	if s.recusar && (desfecho == Divergente || desfecho == Ausente || desfecho == Malformada) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"assinatura invalida"}`)
		return
	}

	if s.forward == "" {
		// 200 sempre: sem destino, queremos o payload, não exercitar o retry
		// de 3 dias do Bling.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}

	status, resposta, err := entregar(r.Context(), ev, s.forward)
	if err != nil {
		// A aplicação estar fora não pode virar 5xx para o Bling: ele
		// re-entregaria por três dias, e o payload já está gravado aqui.
		fmt.Printf("   ⚠ encaminhamento falhou: %v (respondendo 200 assim mesmo)\n", err)
		_ = s.audit.Append(audit.Entry{Kind: "webhook", URL: s.forward, Note: ev.ID, Error: err.Error()})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"forwarded":false}`)
		return
	}

	resumo := strings.TrimSpace(resposta)
	if len(resumo) > 160 {
		resumo = resumo[:160] + "…"
	}
	fmt.Printf("   → app respondeu %d  %s\n", status, resumo)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resposta)
}

func entregar(ctx context.Context, ev *Evento, destino string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, ev.Method, destino, strings.NewReader(ev.Body))
	if err != nil {
		return 0, "", err
	}
	req.Header = ev.Headers.Clone()
	req.Header.Del("Host")
	req.Header.Del("Content-Length")
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// 4 s: a aplicação tem de responder dentro do orçamento de 5 s do Bling,
	// e a folga é nossa margem para responder alguma coisa.
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out), nil
}

// Reenviar repete um evento gravado contra um destino, N vezes, RE-ASSINANDO
// com o secret local. É como se testa idempotência e a verificação de HMAC sem
// depender de o Bling entregar de novo.
func Reenviar(ctx context.Context, ev *Evento, destino, secret string, vezes int) error {
	if vezes < 1 {
		vezes = 1
	}
	cli := &http.Client{Timeout: 30 * time.Second}
	corpo := []byte(ev.Body)

	for i := 1; i <= vezes; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, destino, strings.NewReader(ev.Body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set(HeaderAssinatura, Assinar(corpo, secret))
		} else if v := ev.Headers.Get(HeaderAssinatura); v != "" {
			req.Header.Set(HeaderAssinatura, v)
		}

		iniciou := time.Now()
		resp, err := cli.Do(req)
		if err != nil {
			return fmt.Errorf("replay %d/%d falhou: %w", i, vezes, err)
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		t := strings.TrimSpace(string(out))
		if len(t) > 200 {
			t = t[:200] + "…"
		}
		fmt.Printf("→ %d/%d  HTTP %d  (%s)  %s\n", i, vezes, resp.StatusCode, time.Since(iniciou).Round(time.Millisecond), t)
	}
	return nil
}
