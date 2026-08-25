package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/livecart/tiny-lab/internal/audit"
	"github.com/livecart/tiny-lab/internal/config"
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

// tokenResponse é o corpo devolvido pelo Keycloak nos dois grant types.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (r tokenResponse) toTokens() *Tokens {
	now := time.Now()
	t := &Tokens{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		TokenType:    r.TokenType,
		Scope:        r.Scope,
		ObtainedAt:   now,
		ExpiresAt:    now.Add(time.Duration(r.ExpiresIn) * time.Second),
	}
	if r.RefreshExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(r.RefreshExpiresIn) * time.Second)
	}
	return t
}

// Login roda o fluxo authorization_code inteiro: sobe um servidor efêmero na
// porta do redirect_uri, abre o navegador, espera o callback e troca o code.
func (c *Client) Login(ctx context.Context, noBrowser bool) (*Tokens, error) {
	redirect, err := url.Parse(c.cfg.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("TINY_REDIRECT_URI inválido: %w", err)
	}
	host := redirect.Host
	if redirect.Port() == "" {
		host = net.JoinHostPort(redirect.Hostname(), "80")
	}

	ln, err := net.Listen("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("não consegui escutar em %s (o redirect_uri precisa apontar para uma porta livre desta máquina): %w", host, err)
	}
	defer ln.Close()

	state := randomURLSafe(24)
	verifier := randomURLSafe(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	params := url.Values{
		"client_id":     {c.cfg.ClientID},
		"redirect_uri":  {c.cfg.RedirectURI},
		"response_type": {"code"},
		"scope":         {"openid"},
		"state":         {state},
	}
	if c.cfg.PKCE {
		params.Set("code_challenge", challenge)
		params.Set("code_challenge_method", "S256")
	}
	authURL := c.cfg.AuthURL + "?" + params.Encode()

	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			msg := fmt.Sprintf("%s: %s", e, q.Get("error_description"))
			respondHTML(w, "Autorização recusada", msg)
			result <- callback{err: fmt.Errorf("o Tiny recusou a autorização — %s", msg)}
			return
		}
		if got := q.Get("state"); got != state {
			respondHTML(w, "State inválido", "A resposta não corresponde a esta sessão de login.")
			result <- callback{err: fmt.Errorf("state divergente: possível CSRF, login abortado")}
			return
		}
		code := q.Get("code")
		if code == "" {
			respondHTML(w, "Sem código", "O Tiny respondeu sem authorization code.")
			result <- callback{err: fmt.Errorf("callback sem parâmetro code")}
			return
		}
		respondHTML(w, "Pronto", "Token obtido. Pode fechar esta aba e voltar ao terminal.")
		result <- callback{code: code}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Println("Abra esta URL para autorizar o tiny-lab na conta de teste:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Printf("Aguardando o callback em %s ...\n", c.cfg.RedirectURI)
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
		form := url.Values{
			"grant_type":   {"authorization_code"},
			"code":         {cb.code},
			"redirect_uri": {c.cfg.RedirectURI},
		}
		if c.cfg.PKCE {
			form.Set("code_verifier", verifier)
		}
		return c.exchange(waitCtx, form)
	}
}

// Refresh troca o refresh_token por um par novo.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	return c.exchange(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) exchange(ctx context.Context, form url.Values) (*Tokens, error) {
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		_ = c.audit.Append(audit.Entry{Kind: "oauth", Method: "POST", URL: c.cfg.TokenURL, Note: form.Get("grant_type"), Error: err.Error()})
		return nil, err
	}
	defer resp.Body.Close()

	var tr tokenResponse
	dec := json.NewDecoder(resp.Body)
	decErr := dec.Decode(&tr)

	// O corpo carrega o token; auditamos só o que é seguro guardar.
	_ = c.audit.Append(audit.Entry{
		Kind:       "oauth",
		Method:     "POST",
		URL:        c.cfg.TokenURL,
		Status:     resp.StatusCode,
		DurationMS: time.Since(started).Milliseconds(),
		Note:       fmt.Sprintf("grant_type=%s expires_in=%d scope=%q", form.Get("grant_type"), tr.ExpiresIn, tr.Scope),
		Error:      strings.TrimSpace(tr.Error + " " + tr.ErrorDescription),
	})

	if resp.StatusCode != http.StatusOK {
		if tr.Error != "" {
			hint := ""
			low := strings.ToLower(tr.Error + " " + tr.ErrorDescription)
			switch {
			case strings.Contains(low, "pkce"), strings.Contains(low, "code_challenge"), strings.Contains(low, "code_verifier"):
				hint = "\n  dica: este client parece não aceitar PKCE — rode com TINY_PKCE=false"
			case strings.Contains(low, "redirect"):
				hint = "\n  dica: cadastre " + c.cfg.RedirectURI + " como redirect URI válido na aplicação do Tiny"
			}
			return nil, fmt.Errorf("token endpoint devolveu %d: %s — %s%s", resp.StatusCode, tr.Error, tr.ErrorDescription, hint)
		}
		return nil, fmt.Errorf("token endpoint devolveu %d", resp.StatusCode)
	}
	if decErr != nil {
		return nil, fmt.Errorf("resposta do token endpoint não é JSON: %w", decErr)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint respondeu 200 sem access_token")
	}
	return tr.toTokens(), nil
}

// EnsureFresh devolve um token válido, renovando e persistindo se preciso.
// É o ponto único por onde todo comando obtém credencial.
func (c *Client) EnsureFresh(ctx context.Context) (*Tokens, error) {
	t, err := LoadTokens(c.cfg.TokensPath())
	if err != nil {
		return nil, err
	}
	if !t.Expired() {
		return t, nil
	}
	if !t.RefreshUsable() {
		return nil, fmt.Errorf("access token vencido e refresh token expirado — rode `tiny-lab auth login` de novo")
	}

	fresh, err := c.Refresh(ctx, t.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("renovando token: %w", err)
	}
	fresh.AccountCNPJ, fresh.AccountName = t.AccountCNPJ, t.AccountName
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
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>tiny-lab</title>`+
		`<body style="font:16px system-ui;padding:3rem;max-width:40rem;margin:auto">`+
		`<h1>%s</h1><p>%s</p></body>`, title, msg)
}

// openBrowser é best-effort: em WSL o navegador é o do Windows.
func openBrowser(u string) {
	candidates := [][]string{}
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, []string{"open", u})
	case "windows":
		candidates = append(candidates, []string{"rundll32", "url.dll,FileProtocolHandler", u})
	default:
		// WSL: wslview/cmd.exe alcançam o navegador do Windows; xdg-open cobre Linux puro.
		candidates = append(candidates,
			[]string{"wslview", u},
			[]string{"xdg-open", u},
			[]string{"cmd.exe", "/c", "start", "", strings.ReplaceAll(u, "&", "^&")},
		)
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if err := exec.Command(c[0], c[1:]...).Start(); err == nil {
			return
		}
	}
}
