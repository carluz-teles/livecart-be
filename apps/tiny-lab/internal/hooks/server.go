package hooks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/livecart/tiny-lab/internal/audit"
)

type Server struct {
	store *Store
	audit *audit.Log
	port  int
	seq   atomic.Int64
}

func NewServer(store *Store, lg *audit.Log, port int) *Server {
	return &Server{store: store, audit: lg, port: port}
}

// Serve sobe o receptor. O handler é deliberadamente um catch-all: o painel do
// Tiny pode entregar em qualquer caminho, e perder uma entrega por causa de um
// 404 nosso seria o pior resultado possível numa fase de descoberta.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", s.capture)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("ponte de webhooks ouvindo em http://localhost:%d\n", s.port)
	fmt.Printf("gravando em %s\n", s.store.Dir())
	fmt.Println("qualquer caminho é aceito — o caminho recebido fica gravado no evento.")
	fmt.Println()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "erro lendo corpo", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	now := time.Now().UTC()
	ev := &Event{
		ID:         fmt.Sprintf("%s-%03d", now.Format("20060102T150405.000"), s.seq.Add(1)),
		ReceivedAt: now,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Headers:    r.Header.Clone(),
		Body:       string(body),
		RemoteAddr: r.RemoteAddr,
	}

	if err := s.store.Save(ev); err != nil {
		fmt.Printf("  ✗ FALHA AO GRAVAR o webhook %s: %v\n", ev.ID, err)
	}
	_ = s.audit.Append(audit.Entry{
		Kind: "webhook", Method: ev.Method, URL: ev.Path,
		ResponseRaw: ev.Summary(), Note: ev.ID,
	})

	fmt.Printf("← %s  %s %s\n   %s\n", now.Format("15:04:05.000"), ev.Method, ev.Path, ev.Summary())

	// 200 sempre: nesta fase queremos o payload, não exercitar o retry do Tiny.
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// Replay reenvia um evento gravado para um destino, N vezes. É como se testa
// idempotência: o mesmo byte a byte, repetido.
func Replay(ctx context.Context, ev *Event, target string, times int) error {
	if times < 1 {
		times = 1
	}
	client := &http.Client{Timeout: 30 * time.Second}

	for i := 1; i <= times; i++ {
		req, err := http.NewRequestWithContext(ctx, ev.Method, target, strings.NewReader(ev.Body))
		if err != nil {
			return err
		}
		req.Header = headersToHTTP(ev.Headers)
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("replay %d/%d falhou: %w", i, times, err)
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		trimmed := strings.TrimSpace(string(out))
		if len(trimmed) > 200 {
			trimmed = trimmed[:200] + "…"
		}
		fmt.Printf("→ %d/%d  HTTP %d  (%s)  %s\n", i, times, resp.StatusCode, time.Since(started).Round(time.Millisecond), trimmed)
	}
	return nil
}
