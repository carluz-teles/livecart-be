// Package oauth implementa o fluxo authorization_code do Bling v3.
//
// Três diferenças em relação ao Tiny que NÃO são detalhe de implementação:
//
//  1. As credenciais vão no header Basic e a doc é explícita: "não é permitida
//     a inserção destes parâmetros no body". Mandar no body devolve
//     invalid_client mesmo com credencial correta.
//  2. O authorization code vive UM MINUTO, e a doc avisa que reusar um code
//     ainda válido faz o usuário ter "o seu acesso revogado por medidas de
//     segurança". Por isso a troca do code NÃO tem retry: uma segunda tentativa
//     não é uma chance a mais, é o risco de desconectar o lojista.
//  3. redirect_uri e scope enviados na requisição são IGNORADOS — valem sempre
//     os do cadastro do aplicativo. Um redirect divergente não dá erro na hora;
//     dá um callback que nunca chega.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
	"github.com/livecart/bling-lab/internal/config"
)

type Client struct {
	cfg   *config.Config
	http  *http.Client
	audit *audit.Log
}

func NewClient(cfg *config.Config, lg *audit.Log) *Client {
	return &Client{
		cfg:   cfg,
		http:  &http.Client{Timeout: 30 * time.Second},
		audit: lg,
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

// erroBling é o envelope de erro da API v3, medido em 29/08/2026 contra o
// token endpoint sem header Basic:
//
//	{"error":{"type":"invalid_client","message":"invalid_client",
//	          "description":"Client credentials were not found in the headers"}}
type erroBling struct {
	Error struct {
		Type        string `json:"type"`
		Message     string `json:"message"`
		Description string `json:"description"`
	} `json:"error"`
}

func (e erroBling) String() string {
	if e.Error.Type == "" && e.Error.Description == "" {
		return ""
	}
	if e.Error.Description != "" && e.Error.Description != e.Error.Type {
		return e.Error.Type + ": " + e.Error.Description
	}
	return e.Error.Type
}

// AuthorizeURL monta a URL de autorização. Só três parâmetros importam.
func (c *Client) AuthorizeURL(state string) string {
	p := url.Values{
		"response_type": {"code"},
		"client_id":     {c.cfg.ClientID},
		"state":         {state},
	}
	return c.cfg.AuthURL + "?" + p.Encode()
}

// Login roda o fluxo inteiro: sobe um servidor efêmero, abre o navegador,
// espera o callback e troca o code imediatamente.
func (c *Client) Login(ctx context.Context, noBrowser bool) (*Tokens, error) {
	porta := c.cfg.ListenPort()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", porta))
	if err != nil {
		return nil, fmt.Errorf("não consegui escutar em 127.0.0.1:%d: %w", porta, err)
	}
	defer ln.Close()

	state := randomURLSafe(24)
	authURL := c.AuthorizeURL(state)
	caminho := c.cfg.CallbackPath()

	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)

	mux := http.NewServeMux()
	// Catch-all: se o redirect cadastrado no Bling divergir do caminho que
	// configuramos, é melhor capturar o code do que devolver 404 e perder a
	// autorização — o code vive um minuto e não se pede outro sem o lojista
	// clicar de novo.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != caminho {
			fmt.Printf("  ⚠ callback chegou em %s, mas o esperado era %s — capturando assim mesmo\n", r.URL.Path, caminho)
		}
		if e := q.Get("error"); e != "" {
			msg := strings.TrimSpace(e + " " + q.Get("error_description"))
			respondHTML(w, "Autorização recusada", msg)
			result <- callback{err: fmt.Errorf("o Bling recusou a autorização — %s", msg)}
			return
		}
		if got := q.Get("state"); got != state {
			respondHTML(w, "State inválido", "A resposta não corresponde a esta sessão de login.")
			result <- callback{err: fmt.Errorf("state divergente: possível CSRF, login abortado")}
			return
		}
		code := q.Get("code")
		if code == "" {
			respondHTML(w, "Sem código", "O Bling respondeu sem authorization code.")
			result <- callback{err: fmt.Errorf("callback sem parâmetro code")}
			return
		}
		respondHTML(w, "Pronto", "Autorização recebida. Pode fechar esta aba e voltar ao terminal.")
		result <- callback{code: code}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()

	fmt.Println()
	fmt.Println("Abra esta URL para autorizar o bling-lab na conta:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Printf("redirect cadastrado   %s\n", c.cfg.RedirectURI)
	fmt.Printf("escutando localmente  127.0.0.1:%d%s\n", porta, caminho)
	if !c.cfg.RedirectIsLocal() {
		fmt.Println()
		fmt.Println("  O redirect cadastrado NÃO é localhost — o túnel precisa estar de pé")
		fmt.Printf("  e apontando para a porta %d desta máquina.\n", porta)
	}
	fmt.Println()
	fmt.Println("O code do Bling vale 1 MINUTO. Autorize sem deixar a aba parada.")
	fmt.Println()

	if !noBrowser {
		openBrowser(authURL)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return nil, fmt.Errorf("tempo esgotado esperando a autorização (5 min)")
	case cb := <-result:
		if cb.err != nil {
			return nil, cb.err
		}
		fmt.Println("✓ code recebido, trocando por token AGORA (vale 1 min)…")
		// Contexto próprio e curto: se o ctx do login já estiver perto do fim,
		// a troca não pode herdar um prazo que a faça morrer no meio.
		exCtx, exCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer exCancel()
		t, err := c.exchange(exCtx, url.Values{
			"grant_type": {"authorization_code"},
			"code":       {cb.code},
		})
		if err != nil {
			return nil, fmt.Errorf("%w\n\n  ATENÇÃO: não repita o login imediatamente com o mesmo code.\n"+
				"  A doc do Bling avisa que reusar um code válido REVOGA o acesso do usuário.\n"+
				"  Rode `auth login` de novo e autorize pelo navegador outra vez.", err)
		}
		t.RefreshObtainedAt = t.ObtainedAt
		return t, nil
	}
}

// Refresh troca o refresh_token por um par novo e REGISTRA se ele rotacionou —
// é a medição que decide se a loja parada 30 dias perde a conexão.
func (c *Client) Refresh(ctx context.Context, antigo *Tokens) (*Tokens, error) {
	novo, err := c.exchange(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {antigo.RefreshToken},
	})
	if err != nil {
		return nil, err
	}

	rotacionou := novo.RefreshToken != "" && novo.RefreshToken != antigo.RefreshToken
	novo.Rotacionou = &rotacionou
	if rotacionou {
		novo.RefreshObtainedAt = novo.ObtainedAt
	} else {
		// Não rotacionou: o relógio dos 30 dias continua correndo desde a
		// autorização original. É o caso ruim, e é o que o alerta proativo
		// precisa saber.
		novo.RefreshObtainedAt = antigo.RefreshObtainedAt
		if novo.RefreshToken == "" {
			novo.RefreshToken = antigo.RefreshToken
		}
	}
	novo.CompanyID, novo.CompanyName, novo.CompanyDoc = antigo.CompanyID, antigo.CompanyName, antigo.CompanyDoc
	return novo, nil
}

// Revoke invalida um token. Deliberadamente NÃO expõe revoke_action /
// revoke_target: a doc diz que eles revogam TODOS os tokens de um usuário ou
// empresa, o que num app compartilhado derrubaria lojas que não são esta.
func (c *Client) Revoke(ctx context.Context, token, hint string) error {
	form := url.Values{"token": {token}}
	if hint != "" {
		form.Set("token_type_hint", hint)
	}

	req, err := c.novaRequisicaoBasic(ctx, c.cfg.RevokeURL, form)
	if err != nil {
		return err
	}
	iniciou := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		_ = c.audit.Append(audit.Entry{Kind: "oauth", Method: "POST", URL: c.cfg.RevokeURL, Note: "revoke", Error: err.Error()})
		return err
	}
	defer resp.Body.Close()
	corpo, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	_ = c.audit.Append(audit.Entry{
		Kind: "oauth", Method: "POST", URL: c.cfg.RevokeURL, Status: resp.StatusCode,
		DurationMS: time.Since(iniciou).Milliseconds(), Headers: audit.Headers(resp.Header),
		Note: "revoke token_type_hint=" + hint, ResponseRaw: strings.TrimSpace(string(corpo)),
	})

	if resp.StatusCode >= 300 {
		return fmt.Errorf("revoke devolveu %d: %s", resp.StatusCode, strings.TrimSpace(string(corpo)))
	}
	return nil
}

// novaRequisicaoBasic monta o POST do jeito que o Bling exige: form no corpo,
// credenciais SÓ no header Basic.
func (c *Client) novaRequisicaoBasic(ctx context.Context, endpoint string, form url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.cfg.ClientID + ":" + c.cfg.ClientSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// A doc anuncia a descontinuação do token opaco em favor de JWT. O header
	// pede explicitamente o modelo novo, para não sermos migrados de surpresa.
	req.Header.Set("enable-jwt", "1")
	return req, nil
}

func (c *Client) exchange(ctx context.Context, form url.Values) (*Tokens, error) {
	req, err := c.novaRequisicaoBasic(ctx, c.cfg.TokenURL, form)
	if err != nil {
		return nil, err
	}

	iniciou := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		_ = c.audit.Append(audit.Entry{Kind: "oauth", Method: "POST", URL: c.cfg.TokenURL, Note: form.Get("grant_type"), Error: err.Error()})
		return nil, err
	}
	defer resp.Body.Close()
	corpo, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr tokenResponse
	var eb erroBling
	_ = json.Unmarshal(corpo, &tr)
	_ = json.Unmarshal(corpo, &eb)

	// O corpo carrega o token; auditamos só o que é seguro guardar. Os headers
	// vão inteiros porque provar a ausência de header de cota é um resultado.
	_ = c.audit.Append(audit.Entry{
		Kind:       "oauth",
		Method:     "POST",
		URL:        c.cfg.TokenURL,
		Status:     resp.StatusCode,
		DurationMS: time.Since(iniciou).Milliseconds(),
		Headers:    audit.Headers(resp.Header),
		Note:       fmt.Sprintf("grant_type=%s expires_in=%d scope=%q", form.Get("grant_type"), tr.ExpiresIn, tr.Scope),
		Error:      eb.String(),
	})

	if resp.StatusCode != http.StatusOK {
		msg := eb.String()
		if msg == "" {
			msg = strings.TrimSpace(string(corpo))
		}
		return nil, fmt.Errorf("token endpoint devolveu %d: %s%s", resp.StatusCode, msg, dica(msg))
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint respondeu 200 sem access_token: %s", strings.TrimSpace(string(corpo)))
	}

	agora := time.Now()
	return &Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ObtainedAt:   agora,
		ExpiresAt:    agora.Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// dica traduz os erros que custam tempo quando não se sabe o que significam.
func dica(msg string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "credentials were not found in the headers"):
		return "\n  dica: as credenciais têm de ir no header Basic, nunca no body. Se você vê isto, o header não saiu."
	case strings.Contains(l, "invalid_client"):
		return "\n  dica: confira BLING_CLIENT_ID/BLING_CLIENT_SECRET — são os do aplicativo, não os da conta."
	case strings.Contains(l, "invalid_grant"):
		return "\n  dica: o code expirou (vale 1 minuto) ou já foi usado. Refaça o login pelo navegador."
	case strings.Contains(l, "redirect"):
		return "\n  dica: o redirect_uri CADASTRADO no aplicativo tem de bater com o do túnel/localhost."
	}
	return ""
}

// EnsureFresh devolve um token válido, renovando e persistindo se preciso.
func (c *Client) EnsureFresh(ctx context.Context) (*Tokens, error) {
	t, err := LoadTokens(c.cfg.TokensPath())
	if err != nil {
		return nil, err
	}
	if !t.Expired() {
		return t, nil
	}
	if !t.RefreshUsable() {
		return nil, fmt.Errorf("access token vencido e refresh token expirado (emitido em %s) — rode `bling-lab auth login` de novo",
			t.RefreshObtainedAt.Format(time.RFC3339))
	}

	fresh, err := c.Refresh(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("renovando token: %w", err)
	}
	if err := SaveTokens(c.cfg.TokensPath(), fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func randomURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand indisponível: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func respondHTML(w http.ResponseWriter, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>bling-lab</title>`+
		`<body style="font:16px system-ui;padding:3rem;max-width:40rem;margin:auto">`+
		`<h1>%s</h1><p>%s</p></body>`, title, msg)
}

// openBrowser é best-effort: em WSL o navegador é o do Windows.
func openBrowser(u string) {
	var candidatos [][]string
	switch runtime.GOOS {
	case "darwin":
		candidatos = append(candidatos, []string{"open", u})
	case "windows":
		candidatos = append(candidatos, []string{"rundll32", "url.dll,FileProtocolHandler", u})
	default:
		candidatos = append(candidatos,
			[]string{"wslview", u},
			[]string{"xdg-open", u},
			[]string{"cmd.exe", "/c", "start", "", strings.ReplaceAll(u, "&", "^&")},
		)
	}
	for _, c := range candidatos {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if err := exec.Command(c[0], c[1:]...).Start(); err == nil {
			return
		}
	}
}
