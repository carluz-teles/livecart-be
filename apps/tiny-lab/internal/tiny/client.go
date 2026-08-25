// Package tiny é o cliente da API v3 do Tiny/Olist usado pelos comandos
// exploratórios. Ele resolve o token, aplica o guard de conta em toda escrita,
// respeita 429 e audita cada chamada.
package tiny

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/livecart/tiny-lab/internal/audit"
	"github.com/livecart/tiny-lab/internal/config"
	"github.com/livecart/tiny-lab/internal/oauth"
)

// Info é o subconjunto de GET /info que importa para o guard.
type Info struct {
	RazaoSocial string `json:"razaoSocial"`
	CpfCnpj     string `json:"cpfCnpj"`
	Fantasia    string `json:"fantasia"`
}

type Response struct {
	Status     int
	Header     http.Header
	Body       []byte
	DurationMS int64
	Attempts   int
}

// IsJSON permite ao chamador decidir se formata o corpo ou imprime cru.
func (r *Response) IsJSON() bool {
	return json.Valid(bytes.TrimSpace(r.Body)) && len(bytes.TrimSpace(r.Body)) > 0
}

type Client struct {
	cfg   *config.Config
	oauth *oauth.Client
	http  *http.Client
	audit *audit.Log

	// info é o resultado memoizado de GET /info nesta execução. O guard nunca
	// confia num valor de disco: a conta é reconfirmada uma vez por processo.
	info *Info
}

func New(cfg *config.Config, oc *oauth.Client, lg *audit.Log) *Client {
	return &Client{
		cfg:   cfg,
		oauth: oc,
		http:  &http.Client{Timeout: 60 * time.Second},
		audit: lg,
	}
}

// GuardError sinaliza que a chamada foi BLOQUEADA antes de sair da máquina.
type GuardError struct{ Reason string }

func (e *GuardError) Error() string { return e.Reason }

func isWrite(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// Info busca (e memoiza) a identidade da conta em que o token manda.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	if c.info != nil {
		return c.info, nil
	}
	resp, err := c.do(ctx, http.MethodGet, "/info", nil, nil, true)
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("GET /info devolveu %d: %s", resp.Status, strings.TrimSpace(string(resp.Body)))
	}
	var in Info
	if err := json.Unmarshal(resp.Body, &in); err != nil {
		return nil, fmt.Errorf("GET /info não é o JSON esperado: %w", err)
	}
	c.info = &in

	// Espelha no token store só para `auth status` mostrar sem gastar request.
	if t, err := oauth.LoadTokens(c.cfg.TokensPath()); err == nil {
		t.AccountCNPJ, t.AccountName = config.OnlyDigits(in.CpfCnpj), firstNonEmpty(in.Fantasia, in.RazaoSocial)
		_ = oauth.SaveTokens(c.cfg.TokensPath(), t)
	}
	return c.info, nil
}

// guardWrite é o portão duplo. Falhar aqui é o comportamento desejado: nenhuma
// escrita sai sem que a conta de destino tenha sido confirmada NESTA execução.
func (c *Client) guardWrite(ctx context.Context, method, path string) error {
	if !c.cfg.WriteAllowed() {
		return &GuardError{Reason: fmt.Sprintf(
			"escrita bloqueada: TINY_ENV=%q (só %q permite escrever). %s %s não foi enviado.",
			c.cfg.Env, "sandbox", method, path)}
	}
	if len(c.cfg.AllowedCNPJ) == 0 {
		return &GuardError{Reason: fmt.Sprintf(
			"escrita bloqueada: TINY_ALLOWED_CNPJ está vazio. Sem allowlist não há como provar que o token "+
				"não é de uma conta de produção. %s %s não foi enviado.", method, path)}
	}

	in, err := c.Info(ctx)
	if err != nil {
		return &GuardError{Reason: fmt.Sprintf(
			"escrita bloqueada: não consegui confirmar a conta via GET /info (%v). %s %s não foi enviado.",
			err, method, path)}
	}

	got := config.OnlyDigits(in.CpfCnpj)
	for _, allowed := range c.cfg.AllowedCNPJ {
		if got == allowed {
			return nil
		}
	}
	_ = c.audit.Append(audit.Entry{
		Kind: "guard", Method: method, URL: path, Account: got,
		Error: "conta fora da allowlist — escrita bloqueada",
	})
	return &GuardError{Reason: fmt.Sprintf(
		"escrita bloqueada: o token manda na conta %s (%s), que NÃO está em TINY_ALLOWED_CNPJ (%s). %s %s não foi enviado.",
		got, firstNonEmpty(in.Fantasia, in.RazaoSocial), strings.Join(c.cfg.AllowedCNPJ, ","), method, path)}
}

// Do é a porta de entrada pública: aplica o guard e delega.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) (*Response, error) {
	if isWrite(method) {
		if err := c.guardWrite(ctx, method, path); err != nil {
			return nil, err
		}
	}
	return c.do(ctx, method, path, query, body, false)
}

// do executa de fato, com retry em 429 e 5xx. skipGuard é usado só pelo
// próprio GET /info do guard, para não haver recursão.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, skipGuard bool) (*Response, error) {
	_ = skipGuard

	full := c.cfg.APIBase + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(full, "?") {
			sep = "&"
		}
		full += sep + query.Encode()
	}

	tok, err := c.oauth.EnsureFresh(ctx)
	if err != nil {
		return nil, err
	}

	const maxAttempts = 4
	var last *Response

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		started := time.Now()

		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), full, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			_ = c.audit.Append(audit.Entry{
				Kind: "api", Method: strings.ToUpper(method), URL: full,
				DurationMS: time.Since(started).Milliseconds(), Error: err.Error(),
			})
			return nil, err
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}

		out := &Response{
			Status: resp.StatusCode, Header: resp.Header, Body: raw,
			DurationMS: time.Since(started).Milliseconds(), Attempts: attempt,
		}
		c.record(out, method, full, body)
		last = out

		// 429 e 5xx são os únicos casos de retry. Um 4xx de validação é uma
		// resposta legítima do ERP e tem que chegar intacta a quem explora.
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return out, nil
		}
		if attempt == maxAttempts {
			return out, nil
		}

		wait := retryAfter(resp.Header, attempt)
		fmt.Printf("  ↻ HTTP %d — aguardando %s antes da tentativa %d/%d\n", resp.StatusCode, wait, attempt+1, maxAttempts)
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(wait):
		}
	}
	return last, nil
}

func (c *Client) record(r *Response, method, full string, body []byte) {
	e := audit.Entry{
		Kind: "api", Method: strings.ToUpper(method), URL: full,
		Status: r.Status, DurationMS: r.DurationMS,
		Headers: interestingHeaders(r.Header),
	}
	if c.info != nil {
		e.Account = config.OnlyDigits(c.info.CpfCnpj)
	}
	if json.Valid(body) {
		e.RequestBody = json.RawMessage(body)
	}
	if trimmed := bytes.TrimSpace(r.Body); json.Valid(trimmed) && len(trimmed) > 0 {
		e.ResponseBody = json.RawMessage(trimmed)
	} else if len(trimmed) > 0 {
		e.ResponseRaw = string(trimmed)
	}
	_ = c.audit.Append(e)
}

// retryAfter respeita o header quando ele vem; senão, backoff exponencial.
// Descobrir qual dos dois o Tiny usa é uma das perguntas da Fase 0, e o
// audit.jsonl guarda os headers justamente para respondê-la com evidência.
func retryAfter(h http.Header, attempt int) time.Duration {
	for _, key := range []string{"Retry-After", "X-RateLimit-Reset", "RateLimit-Reset"} {
		if v := h.Get(key); v != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 && secs <= 300 {
				return time.Duration(secs) * time.Second
			}
			if when, err := http.ParseTime(v); err == nil {
				if d := time.Until(when); d > 0 && d <= 5*time.Minute {
					return d
				}
			}
		}
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// interestingHeaders guarda o que ajuda a caracterizar rate limit e tracing, e
// nada que possa carregar credencial.
func interestingHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "rate-limit") ||
			lk == "retry-after" || lk == "x-request-id" || lk == "x-correlation-id" ||
			lk == "content-type" || lk == "date" {
			out[k] = strings.Join(v, ", ")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
