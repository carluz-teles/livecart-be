// Package bling é o cliente HTTP do laboratório: um freio previsível, um guard
// de escrita e auditoria completa de headers.
//
// O freio é PREDITIVO e não adaptativo, e isso é consequência de medição, não
// preferência: em 29/08/2026 o dump completo dos headers de uma resposta 400 do
// token endpoint e de uma 401 da API não trouxe X-RateLimit-*, Retry-After nem
// RateLimit-*. A API está atrás de AWS API Gateway + Cloudflare. Não há nada
// para reconciliar — só para prever. Um limitador que espera header (como o
// AdaptiveLimiter do LiveCart) passaria a vida inteira sem freio.
package bling

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
	"github.com/livecart/bling-lab/internal/config"
	"github.com/livecart/bling-lab/internal/oauth"
)

type Client struct {
	cfg   *config.Config
	oauth *oauth.Client
	http  *http.Client
	audit *audit.Log

	mu        sync.Mutex
	ultima    time.Time
	intervalo time.Duration

	// chamadas conta as requisições desta execução, para o relatório de custo.
	chamadas int
}

func New(cfg *config.Config, oc *oauth.Client, lg *audit.Log) *Client {
	return &Client{
		cfg:       cfg,
		oauth:     oc,
		http:      &http.Client{Timeout: 45 * time.Second},
		audit:     lg,
		intervalo: time.Duration(float64(time.Second) / cfg.RateLimitRPS),
	}
}

// Chamadas devolve quantas requisições esta execução gastou da cota da conta.
func (c *Client) Chamadas() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chamadas
}

// esperarVaga espaça as requisições uniformemente. Simples de propósito: o
// objetivo é não estourar 2-3 req/s numa ferramenta de exploração, não
// reimplementar o pipeline de produção.
func (c *Client) esperarVaga(ctx context.Context) error {
	c.mu.Lock()
	espera := time.Duration(0)
	agora := time.Now()
	if !c.ultima.IsZero() {
		if d := c.intervalo - agora.Sub(c.ultima); d > 0 {
			espera = d
		}
	}
	c.ultima = agora.Add(espera)
	c.chamadas++
	c.mu.Unlock()

	if espera <= 0 {
		return nil
	}
	t := time.NewTimer(espera)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Resposta é o que todo comando recebe: corpo, status e headers crus.
type Resposta struct {
	Status  int
	Headers http.Header
	Body    []byte
	Duracao time.Duration
}

// ErroAPI carrega o envelope de erro do Bling com o status, para o chamador
// distinguir 429 de 401 de 400 sem parsear string.
type ErroAPI struct {
	Status    int
	Tipo      string
	Descricao string
	Corpo     string
	RequestID string // x-amzn-RequestId — é o que se manda para o suporte do Bling
}

func (e *ErroAPI) Error() string {
	msg := e.Tipo
	if e.Descricao != "" && e.Descricao != e.Tipo {
		msg = e.Tipo + ": " + e.Descricao
	}
	if msg == "" {
		msg = strings.TrimSpace(e.Corpo)
	}
	s := fmt.Sprintf("HTTP %d — %s", e.Status, msg)
	if e.RequestID != "" {
		s += " (x-amzn-RequestId: " + e.RequestID + ")"
	}
	if e.Status == http.StatusTooManyRequests {
		s += "\n  429 do Bling: o teto é 3 req/s POR CONTA somando TODOS os apps do lojista.\n" +
			"  Não há Retry-After — se o e-commerce dele também consome, a cota some sem aviso."
	}
	return s
}

// Get é o único verbo que o laboratório usa por padrão. Escrita passa por Write,
// que tem guard.
func (c *Client) Get(ctx context.Context, caminho string, query url.Values) (*Resposta, error) {
	return c.do(ctx, http.MethodGet, caminho, query, nil)
}

// Write executa um verbo de escrita — e só existe para o dia em que o lojista
// autorizar explicitamente. Dois portões, como no tiny-lab: a flag de ambiente
// e a allowlist da conta conferida contra a identidade real.
func (c *Client) Write(ctx context.Context, metodo, caminho string, corpo any) (*Resposta, error) {
	if !c.cfg.AllowWrite {
		_ = c.audit.Append(audit.Entry{Kind: "guard", Method: metodo, URL: caminho,
			Error: "escrita bloqueada: BLING_ALLOW_WRITE não é true"})
		return nil, fmt.Errorf("escrita BLOQUEADA (%s %s).\n"+
			"  O Bling não tem sandbox: isto rodaria contra a conta real do lojista.\n"+
			"  Para liberar, defina BLING_ALLOW_WRITE=true e BLING_ALLOWED_COMPANY_ID=<id da conta>.", metodo, caminho)
	}
	if len(c.cfg.AllowedCompanyID) == 0 {
		return nil, fmt.Errorf("escrita BLOQUEADA: BLING_ALLOW_WRITE=true mas BLING_ALLOWED_COMPANY_ID está vazio.\n" +
			"  A allowlist da conta é o portão que impede escrever na conta errada.")
	}
	emp, err := c.Empresa(ctx)
	if err != nil {
		return nil, fmt.Errorf("não consegui confirmar em qual conta escreveria: %w", err)
	}
	if !contem(c.cfg.AllowedCompanyID, emp.ID) {
		_ = c.audit.Append(audit.Entry{Kind: "guard", Method: metodo, URL: caminho, Account: emp.ID,
			Error: "conta fora da allowlist"})
		return nil, fmt.Errorf("escrita BLOQUEADA: o token é da conta %s (%s), que não está em BLING_ALLOWED_COMPANY_ID",
			emp.ID, emp.Nome)
	}

	var reader io.Reader
	var bruto []byte
	if corpo != nil {
		bruto, err = json.Marshal(corpo)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(bruto))
	}
	return c.doComCorpo(ctx, metodo, caminho, nil, reader, bruto)
}

func (c *Client) do(ctx context.Context, metodo, caminho string, query url.Values, corpo io.Reader) (*Resposta, error) {
	return c.doComCorpo(ctx, metodo, caminho, query, corpo, nil)
}

func (c *Client) doComCorpo(ctx context.Context, metodo, caminho string, query url.Values, corpo io.Reader, bruto []byte) (*Resposta, error) {
	t, err := c.oauth.EnsureFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.esperarVaga(ctx); err != nil {
		return nil, err
	}

	endereco := c.cfg.APIBase + "/" + strings.TrimPrefix(caminho, "/")
	if len(query) > 0 {
		endereco += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, metodo, endereco, corpo)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	req.Header.Set("Accept", "application/json")
	if corpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	iniciou := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		_ = c.audit.Append(audit.Entry{Kind: "api", Method: metodo, URL: endereco, Error: err.Error()})
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	duracao := time.Since(iniciou)

	e := audit.Entry{
		Kind: "api", Method: metodo, URL: endereco, Status: resp.StatusCode,
		DurationMS: duracao.Milliseconds(), Headers: audit.Headers(resp.Header),
		Account: t.CompanyID,
	}
	if json.Valid(bruto) {
		e.RequestBody = bruto
	}
	if json.Valid(body) {
		e.ResponseBody = body
	} else {
		e.ResponseRaw = truncar(string(body), 2000)
	}
	_ = c.audit.Append(e)

	r := &Resposta{Status: resp.StatusCode, Headers: resp.Header, Body: body, Duracao: duracao}
	if resp.StatusCode >= 400 {
		var eb struct {
			Error struct {
				Type        string `json:"type"`
				Message     string `json:"message"`
				Description string `json:"description"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &eb)
		return r, &ErroAPI{
			Status: resp.StatusCode, Tipo: eb.Error.Type, Descricao: eb.Error.Description,
			Corpo: truncar(string(body), 600), RequestID: resp.Header.Get("x-amzn-RequestId"),
		}
	}
	return r, nil
}

func contem(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func truncar(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
