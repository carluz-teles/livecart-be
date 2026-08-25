// tiny-lab — ferramenta local de exploração da API v3 do Tiny/Olist.
//
// Três funções, todas voltadas à Fase 0 da refatoração da integração:
//
//	auth   — fluxo OAuth2 authorization_code contra a conta de teste
//	api    — bate em qualquer endpoint com o token já resolvido
//	hooks  — recebe, grava e reenvia webhooks reais
//
// Toda chamada é auditada em .tiny-lab/audit.jsonl. Toda ESCRITA passa por um
// guard duplo (TINY_ENV=sandbox + allowlist de CNPJ conferida via GET /info).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/livecart/tiny-lab/internal/audit"
	"github.com/livecart/tiny-lab/internal/config"
	"github.com/livecart/tiny-lab/internal/hooks"
	"github.com/livecart/tiny-lab/internal/oauth"
	"github.com/livecart/tiny-lab/internal/tiny"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nerro: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lg, err := audit.New(cfg.AuditPath())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "auth":
		return cmdAuth(ctx, cfg, lg, args[1:])
	case "api":
		return cmdAPI(ctx, cfg, lg, args[1:])
	case "hooks":
		return cmdHooks(ctx, cfg, lg, args[1:])
	case "audit":
		return cmdAudit(cfg, args[1:])
	default:
		usage()
		return fmt.Errorf("comando desconhecido: %q", args[0])
	}
}

// ---------------------------------------------------------------- auth

func cmdAuth(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	oc := oauth.NewClient(cfg, lg)

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "login":
		noBrowser := hasFlag(args, "--no-browser")
		t, err := oc.Login(ctx, noBrowser)
		if err != nil {
			return err
		}
		if err := oauth.SaveTokens(cfg.TokensPath(), t); err != nil {
			return err
		}
		fmt.Printf("\n✓ token salvo em %s (vence em %s)\n", cfg.TokensPath(), time.Until(t.ExpiresAt).Round(time.Second))
		return printAccount(ctx, cfg, oc, lg)

	case "refresh":
		t, err := oauth.LoadTokens(cfg.TokensPath())
		if err != nil {
			return err
		}
		fresh, err := oc.Refresh(ctx, t.RefreshToken)
		if err != nil {
			return err
		}
		fresh.AccountCNPJ, fresh.AccountName = t.AccountCNPJ, t.AccountName
		if err := oauth.SaveTokens(cfg.TokensPath(), fresh); err != nil {
			return err
		}
		fmt.Printf("✓ token renovado (vence em %s)\n", time.Until(fresh.ExpiresAt).Round(time.Second))
		return nil

	case "status", "":
		t, err := oauth.LoadTokens(cfg.TokensPath())
		if err != nil {
			return err
		}
		fmt.Printf("arquivo        %s\n", cfg.TokensPath())
		fmt.Printf("obtido em      %s\n", t.ObtainedAt.Local().Format(time.RFC3339))
		fmt.Printf("access token   %s\n", expiryLabel(t.ExpiresAt))
		fmt.Printf("refresh token  %s\n", expiryLabel(t.RefreshExpiresAt))
		fmt.Printf("escopo         %s\n", orDash(t.Scope))
		fmt.Println()
		fmt.Printf("TINY_ENV       %s (escrita %s)\n", orDash(cfg.Env), allowedLabel(cfg.WriteAllowed()))
		fmt.Printf("allowlist      %s\n", orDash(strings.Join(cfg.AllowedCNPJ, ", ")))
		fmt.Println()
		return printAccount(ctx, cfg, oc, lg)

	default:
		return fmt.Errorf("uso: tiny-lab auth [login|status|refresh]")
	}
}

func printAccount(ctx context.Context, cfg *config.Config, oc *oauth.Client, lg *audit.Log) error {
	in, err := tiny.New(cfg, oc, lg).Info(ctx)
	if err != nil {
		return fmt.Errorf("consultando GET /info: %w", err)
	}
	cnpj := config.OnlyDigits(in.CpfCnpj)

	inList := false
	for _, a := range cfg.AllowedCNPJ {
		if a == cnpj {
			inList = true
		}
	}

	fmt.Printf("conta          %s\n", strings.TrimSpace(in.RazaoSocial))
	fmt.Printf("fantasia       %s\n", orDash(in.Fantasia))
	fmt.Printf("cnpj           %s  %s\n", in.CpfCnpj, allowlistLabel(inList))
	if cfg.WriteAllowed() && !inList {
		fmt.Println()
		fmt.Println("⚠ TINY_ENV=sandbox mas esta conta NÃO está na allowlist — toda escrita será bloqueada.")
		fmt.Printf("  Para liberar, acrescente ao .env:  TINY_ALLOWED_CNPJ=%s\n", cnpj)
	}
	return nil
}

// ---------------------------------------------------------------- api

func cmdAPI(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	if len(args) < 2 {
		return fmt.Errorf("uso: tiny-lab api <MÉTODO> <caminho> [-q chave=valor]... [-d '<json>' | -d @arquivo] [--dry-run] [--raw]")
	}

	method, path := strings.ToUpper(args[0]), args[1]
	query := map[string][]string{}
	var body []byte
	dryRun, raw := false, false

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-q", "--query":
			if i+1 >= len(args) {
				return fmt.Errorf("-q exige chave=valor")
			}
			i++
			k, v, ok := strings.Cut(args[i], "=")
			if !ok {
				return fmt.Errorf("-q %q não está no formato chave=valor", args[i])
			}
			query[k] = append(query[k], v)
		case "-d", "--data":
			if i+1 >= len(args) {
				return fmt.Errorf("-d exige um JSON ou @arquivo")
			}
			i++
			if strings.HasPrefix(args[i], "@") {
				b, err := os.ReadFile(strings.TrimPrefix(args[i], "@"))
				if err != nil {
					return err
				}
				body = b
			} else {
				body = []byte(args[i])
			}
			if !json.Valid(body) {
				return fmt.Errorf("o corpo passado em -d não é JSON válido")
			}
		case "--dry-run":
			dryRun = true
		case "--raw":
			raw = true
		default:
			return fmt.Errorf("argumento desconhecido: %q", args[i])
		}
	}

	fmt.Printf("→ %s %s\n", method, path)
	for k, vs := range query {
		for _, v := range vs {
			fmt.Printf("   ?%s=%s\n", k, v)
		}
	}
	if body != nil {
		fmt.Println("   corpo:")
		fmt.Println(indent(prettyJSON(body), "   "))
	}
	if dryRun {
		fmt.Println("\n(--dry-run: nada foi enviado)")
		return nil
	}

	client := tiny.New(cfg, oauth.NewClient(cfg, lg), lg)
	resp, err := client.Do(ctx, method, path, query, body)
	if err != nil {
		var ge *tiny.GuardError
		if errors.As(err, &ge) {
			fmt.Println()
			fmt.Println("⛔ " + ge.Reason)
			return fmt.Errorf("bloqueado pelo guard")
		}
		return err
	}

	fmt.Printf("\n← HTTP %d  (%dms, tentativa %d)\n", resp.Status, resp.DurationMS, resp.Attempts)
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || lk == "retry-after" || lk == "x-request-id" {
			fmt.Printf("   %s: %s\n", k, strings.Join(v, ", "))
		}
	}
	if len(strings.TrimSpace(string(resp.Body))) == 0 {
		fmt.Println("   (sem corpo)")
	} else if raw || !resp.IsJSON() {
		fmt.Println(string(resp.Body))
	} else {
		fmt.Println(prettyJSON(resp.Body))
	}
	fmt.Printf("\n(auditado em %s)\n", lg.Path())
	return nil
}

// ---------------------------------------------------------------- hooks

func cmdHooks(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	store, err := hooks.NewStore(cfg.HooksDir())
	if err != nil {
		return err
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "serve":
		port := cfg.HooksPort
		if v := flagValue(args, "--port"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("--port inválido: %w", err)
			}
			port = p
		}
		return hooks.NewServer(store, lg, port, flagValue(args, "--forward")).Serve(ctx)

	case "list":
		evs, err := store.List()
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			fmt.Printf("nenhum webhook gravado ainda em %s\n", store.Dir())
			return nil
		}
		for _, e := range evs {
			fmt.Printf("%s  %-22s %s\n", e.ID, e.Tipo(), e.Summary())
		}
		fmt.Printf("\n%d evento(s) em %s\n", len(evs), store.Dir())
		return nil

	case "show":
		if len(args) < 2 {
			return fmt.Errorf("uso: tiny-lab hooks show <id>")
		}
		e, err := store.Get(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("id          %s\n", e.ID)
		fmt.Printf("recebido    %s\n", e.ReceivedAt.Local().Format(time.RFC3339Nano))
		fmt.Printf("requisição  %s %s\n", e.Method, e.Path)
		fmt.Println("headers:")
		for k, v := range e.Headers {
			fmt.Printf("   %s: %s\n", k, strings.Join(v, ", "))
		}
		fmt.Println("corpo:")
		fmt.Println(prettyJSON([]byte(e.Body)))
		return nil

	case "replay":
		if len(args) < 2 {
			return fmt.Errorf("uso: tiny-lab hooks replay <id> [--to <url>] [--times <n>]")
		}
		e, err := store.Get(args[1])
		if err != nil {
			return err
		}
		target := flagValue(args, "--to")
		if target == "" {
			target = fmt.Sprintf("http://localhost:%d%s", cfg.HooksPort, e.Path)
		}
		times := 1
		if v := flagValue(args, "--times"); v != "" {
			if times, err = strconv.Atoi(v); err != nil {
				return fmt.Errorf("--times inválido: %w", err)
			}
		}
		fmt.Printf("reenviando %s (%s) para %s, %d vez(es)\n\n", e.ID, e.Tipo(), target, times)
		return hooks.Replay(ctx, e, target, times)

	default:
		return fmt.Errorf("uso: tiny-lab hooks [serve|list|show|replay]")
	}
}

// ---------------------------------------------------------------- audit

func cmdAudit(cfg *config.Config, args []string) error {
	n := 20
	if len(args) > 1 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			n = v
		}
	}
	b, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("nenhuma chamada auditada ainda (%s)\n", cfg.AuditPath())
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		var e audit.Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			continue
		}
		fmt.Printf("%s  %-8s %-6s %-3d %5dms  %s %s\n",
			e.At.Local().Format("15:04:05"), e.Kind, e.Method, e.Status, e.DurationMS, e.URL, e.Note)
	}
	fmt.Printf("\n(%s)\n", cfg.AuditPath())
	return nil
}

// ---------------------------------------------------------------- utilitários

func usage() {
	fmt.Print(`tiny-lab — exploração da API v3 do Tiny/Olist

  auth login [--no-browser]     autoriza na conta de teste e salva os tokens
  auth status                   mostra token, conta ativa e estado do guard
  auth refresh                  força a renovação do access token

  api <MÉTODO> <caminho> [...]  bate em qualquer endpoint com o token resolvido
      -q chave=valor            query param (repetível)
      -d '<json>' | -d @arq     corpo da requisição
      --dry-run                 imprime o que seria enviado, sem enviar
      --raw                     não formata a resposta

  hooks serve [--port N] [--forward <url>]
                                recebe e grava webhooks (payload cru); com
                                --forward, repassa para a aplicação e devolve
                                ao Tiny a resposta que ela deu
  hooks list                    lista os webhooks gravados
  hooks show <id>               mostra headers e corpo de um webhook
  hooks replay <id> [--to url] [--times n]

  audit [n]                     últimas n linhas do log de auditoria

ESCRITA (POST/PUT/PATCH/DELETE) exige DOIS portões:
  1. TINY_ENV=sandbox
  2. o cpfCnpj devolvido por GET /info tem que estar em TINY_ALLOWED_CNPJ
Não existe bypass — a API v3 tem host único, então a conta é a única fronteira real.
`)
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func prettyJSON(b []byte) string {
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return string(b)
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return string(b)
	}
	return string(pretty)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func expiryLabel(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if d := time.Until(t); d > 0 {
		return fmt.Sprintf("vence em %s (%s)", d.Round(time.Second), t.Local().Format("15:04:05"))
	}
	return fmt.Sprintf("VENCIDO desde %s", t.Local().Format(time.RFC3339))
}

func allowedLabel(ok bool) string {
	if ok {
		return "PERMITIDA"
	}
	return "BLOQUEADA"
}

func allowlistLabel(ok bool) string {
	if ok {
		return "✓ na allowlist"
	}
	return "✗ FORA da allowlist"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
