// bling-lab — ferramenta local de exploração da API v3 do Bling.
//
// Existe porque o Bling NÃO TEM SANDBOX: todo experimento roda contra a conta
// real de um lojista. Daí as três decisões que moldam este binário:
//
//   - só LEITURA por padrão; escrita exige duas chaves (BLING_ALLOW_WRITE e a
//     allowlist da conta conferida contra GET /empresas/me/dados-basicos);
//   - freio de taxa PREDITIVO, porque a API não devolve header de cota nenhum;
//   - auditoria completa dos headers de resposta, porque provar a AUSÊNCIA de
//     header de cota contra a conta real é um dos resultados que o plano usa.
//
// Tudo o que sai daqui é gravado em .bling-lab/audit.jsonl.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
	"github.com/livecart/bling-lab/internal/bling"
	"github.com/livecart/bling-lab/internal/config"
	"github.com/livecart/bling-lab/internal/hooks"
	"github.com/livecart/bling-lab/internal/oauth"
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
	case "empresa":
		return cmdEmpresa(ctx, cfg, lg)
	case "produtos":
		return cmdProdutos(ctx, cfg, lg, args[1:])
	case "produto":
		return cmdProduto(ctx, cfg, lg, args[1:])
	case "variacoes":
		return cmdVariacoes(ctx, cfg, lg, args[1:])
	case "saldos":
		return cmdSaldos(ctx, cfg, lg, args[1:])
	case "depositos":
		return cmdDepositos(ctx, cfg, lg)
	case "balanco":
		return cmdBalanco(ctx, cfg, lg, args[1:])
	case "t1":
		return cmdT1(ctx, cfg, lg, args[1:])
	case "post", "put", "patch", "delete":
		return cmdEscritaCrua(ctx, cfg, lg, strings.ToUpper(args[0]), args[1:])
	case "api":
		return cmdAPI(ctx, cfg, lg, args[1:])
	case "hooks":
		return cmdHooks(ctx, cfg, lg, args[1:])
	case "probe":
		return cmdProbe(ctx, cfg, lg)
	case "audit":
		return cmdAudit(cfg, args[1:])
	case "config":
		return cmdConfig(cfg)
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
		t, err := oc.Login(ctx, temFlag(args, "--no-browser"))
		if err != nil {
			return err
		}
		if err := oauth.SaveTokens(cfg.TokensPath(), t); err != nil {
			return err
		}
		fmt.Printf("\n✓ token salvo em %s\n", cfg.TokensPath())
		fmt.Printf("  access token vence em %s\n", time.Until(t.ExpiresAt).Round(time.Second))
		fmt.Printf("  refresh token vence em ~%s (30 dias, prazo que a doc declara em prosa —\n", time.Until(t.RefreshExpiresAt()).Round(time.Hour))
		fmt.Printf("  o Bling NÃO manda esse prazo na resposta)\n\n")
		return mostrarEmpresa(ctx, cfg, lg, oc)

	case "refresh":
		t, err := oauth.LoadTokens(cfg.TokensPath())
		if err != nil {
			return err
		}
		novo, err := oc.Refresh(ctx, t)
		if err != nil {
			return err
		}
		if err := oauth.SaveTokens(cfg.TokensPath(), novo); err != nil {
			return err
		}
		fmt.Printf("✓ token renovado (vence em %s)\n", time.Until(novo.ExpiresAt).Round(time.Second))
		if novo.Rotacionou != nil {
			if *novo.Rotacionou {
				fmt.Println("  MEDIÇÃO: o refresh token ROTACIONOU — o relógio dos 30 dias reinicia a cada uso.")
				fmt.Println("  Consequência: uma loja que roda live toda semana nunca perde a conexão.")
			} else {
				fmt.Println("  MEDIÇÃO: o refresh token NÃO rotacionou — é o MESMO de antes.")
				fmt.Println("  Consequência: o relógio dos 30 dias corre desde a AUTORIZAÇÃO, e uma loja")
				fmt.Println("  parada 30 dias perde a conexão faça o worker o que fizer. O aviso proativo")
				fmt.Println("  vira obrigatório (dívida D7 do plano).")
			}
		}
		return nil

	case "revoke":
		t, err := oauth.LoadTokens(cfg.TokensPath())
		if err != nil {
			return err
		}
		if !temFlag(args, "--sim") {
			return fmt.Errorf("revoke desconecta esta conta do aplicativo. Confirme com --sim")
		}
		if err := oc.Revoke(ctx, t.RefreshToken, "refresh_token"); err != nil {
			return err
		}
		fmt.Println("✓ refresh token revogado. O access token pode continuar válido até vencer.")
		return nil

	case "status", "":
		t, err := oauth.LoadTokens(cfg.TokensPath())
		if err != nil {
			return err
		}
		fmt.Printf("arquivo            %s\n", cfg.TokensPath())
		fmt.Printf("tipo               %s\n", t.TokenType)
		if t.Scope != "" {
			fmt.Printf("escopos            %s\n", t.Scope)
		}
		fmt.Printf("obtido em          %s\n", t.ObtainedAt.Local().Format(time.RFC3339))
		fmt.Printf("access vence em    %s (%s)\n", t.ExpiresAt.Local().Format(time.RFC3339), restante(time.Until(t.ExpiresAt)))
		fmt.Printf("refresh vence em   ~%s (%s)\n", t.RefreshExpiresAt().Local().Format(time.RFC3339), restante(time.Until(t.RefreshExpiresAt())))
		if t.Rotacionou != nil {
			fmt.Printf("refresh rotaciona  %v (medido)\n", *t.Rotacionou)
		} else {
			fmt.Printf("refresh rotaciona  não medido — rode `auth refresh` para descobrir\n")
		}
		if t.CompanyID != "" {
			fmt.Printf("conta              %s  %s  %s\n", t.CompanyID, t.CompanyName, t.CompanyDoc)
		}
		return nil

	default:
		return fmt.Errorf("subcomando de auth desconhecido: %q (login|status|refresh|revoke)", sub)
	}
}

func restante(d time.Duration) string {
	if d <= 0 {
		return "VENCIDO"
	}
	if d > 48*time.Hour {
		return fmt.Sprintf("faltam %d dias", int(d.Hours()/24))
	}
	return "faltam " + d.Round(time.Second).String()
}

// ---------------------------------------------------------------- empresa

func cmdEmpresa(ctx context.Context, cfg *config.Config, lg *audit.Log) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	return mostrarEmpresa(ctx, cfg, lg, oauth.NewClient(cfg, lg))
}

func mostrarEmpresa(ctx context.Context, cfg *config.Config, lg *audit.Log, oc *oauth.Client) error {
	cli := bling.New(cfg, oc, lg)
	emp, err := cli.Empresa(ctx)
	if err != nil {
		return err
	}
	fmt.Println("conta conectada")
	fmt.Printf("  id (companyId?)  %s\n", emp.ID)
	fmt.Printf("  nome             %s\n", emp.Nome)
	fmt.Printf("  cnpj             %s\n", emp.CNPJ)
	if emp.Email != "" {
		fmt.Printf("  email            %s\n", emp.Email)
	}
	if emp.DataContrato != "" {
		fmt.Printf("  contrato desde   %s\n", emp.DataContrato)
	}
	fmt.Println()
	fmt.Println("  ⚠ que este id seja BYTE-IDÊNTICO ao companyId do webhook ainda NÃO está provado.")
	fmt.Println("    É a premissa da chave de cota E do roteamento por URL única. Rode `hooks serve`,")
	fmt.Println("    provoque um evento no painel do Bling, e compare os dois.")

	// Persiste no tokens.json para os outros comandos e o guard de escrita.
	if t, err := oauth.LoadTokens(cfg.TokensPath()); err == nil {
		t.CompanyID, t.CompanyName, t.CompanyDoc = emp.ID, emp.Nome, emp.CNPJ
		_ = oauth.SaveTokens(cfg.TokensPath(), t)
	}
	return nil
}

// ---------------------------------------------------------------- produtos

func cmdProdutos(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	p := bling.ListarProdutosParams{
		Pagina:               valorInt(args, "--pagina", 1),
		Limite:               valorInt(args, "--limite", 100),
		Criterio:             valorInt(args, "--criterio", 5), // 5 = TODOS. O default do Bling é 1 e esconde produto.
		Tipo:                 valorStr(args, "--tipo", "T"),
		Nome:                 valorStr(args, "--nome", ""),
		DataAlteracaoInicial: valorStr(args, "--alterado-desde", ""),
	}

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	produtos, err := cli.ListarProdutos(ctx, p)
	if err != nil {
		return err
	}

	if temFlag(args, "--json") {
		return imprimirJSON(produtos)
	}

	fmt.Printf("%d produto(s)  (pagina=%d limite=%d criterio=%d tipo=%s)\n\n", len(produtos), p.Pagina, p.Limite, p.Criterio, p.Tipo)
	if len(produtos) == 0 {
		fmt.Println("  Nenhum. Se você esperava produtos aqui, confira o `criterio`:")
		fmt.Println("  1 últimos incluídos (DEFAULT DO BLING) · 2 ativos · 3 inativos · 4 excluídos · 5 todos")
		return nil
	}
	fmt.Printf("%-12s %-10s %-38s %10s %8s %s\n", "ID", "SKU", "NOME", "PREÇO", "SALDO", "T/S/F")
	for _, pr := range produtos {
		fmt.Printf("%-12d %-10s %-38s %10.2f %8.0f %s/%s/%s\n",
			pr.ID, truncar(pr.Codigo, 10), truncar(pr.Nome, 38), pr.Preco,
			pr.Estoque.SaldoVirtualTotal, pr.Tipo, pr.Situacao, pr.Formato)
	}
	fmt.Println()
	fmt.Println("SALDO acima vem de GET /produtos, cujo spec diz 'CONSIDERANDO a reserva de estoque'.")
	fmt.Println("O MESMO campo em GET /estoques/saldos é descrito como 'DESCONSIDERANDO produtos")
	fmt.Println("reservados'. Compare com `bling-lab saldos <ids>` — se divergirem, temos a resposta.")
	fmt.Printf("\n%d requisição(ões) gastas da cota da conta.\n", cli.Chamadas())
	return nil
}

func cmdProduto(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("uso: bling-lab produto <id>")
	}
	ids, err := bling.IDsDeTexto(args[:1])
	if err != nil {
		return err
	}
	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	cru, err := cli.ProdutoCru(ctx, ids[0])
	if err != nil {
		return err
	}
	return imprimirJSONCru(cru)
}

func cmdVariacoes(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("uso: bling-lab variacoes <idProdutoPai>")
	}
	ids, err := bling.IDsDeTexto(args[:1])
	if err != nil {
		return err
	}
	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	cru, err := cli.VariacoesCru(ctx, ids[0])
	if err != nil {
		return err
	}
	return imprimirJSONCru(cru)
}

// ---------------------------------------------------------------- estoque

func cmdSaldos(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	positivos := semFlags(args)
	if len(positivos) == 0 {
		return fmt.Errorf("uso: bling-lab saldos <id> [id...] [--filtro 0|1|2]\n" +
			"  filtro: 0 zerado · 1 positivo (DEFAULT DO BLING) · 2 negativo\n" +
			"  o lab manda 5 por padrão? NÃO — o Bling não tem 'todos'. Ver a nota do comando.")
	}
	ids, err := bling.IDsDeTexto(positivos)
	if err != nil {
		return err
	}
	filtro := valorInt(args, "--filtro", -1)

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	saldos, err := cli.Saldos(ctx, ids, filtro)
	if err != nil {
		return err
	}

	if temFlag(args, "--json") {
		return imprimirJSON(saldos)
	}

	fmt.Printf("%d produto(s) pedido(s), %d devolvido(s)  (uma requisição só — a vantagem real sobre o Tiny)\n\n", len(ids), len(saldos))
	fmt.Printf("%-12s %-12s %14s %14s\n", "ID", "SKU", "FÍSICO", "VIRTUAL")
	vistos := map[int64]bool{}
	for _, s := range saldos {
		vistos[s.Produto.ID] = true
		fmt.Printf("%-12d %-12s %14.2f %14.2f\n", s.Produto.ID, truncar(s.Produto.Codigo, 12), s.SaldoFisicoTotal, s.SaldoVirtualTotal)
		for _, d := range s.Depositos {
			fmt.Printf("   └ depósito %-8d %14.2f %14.2f\n", d.ID, d.SaldoFisico, d.SaldoVirtual)
		}
	}

	var faltando []int64
	for _, id := range ids {
		if !vistos[id] {
			faltando = append(faltando, id)
		}
	}
	if len(faltando) > 0 {
		fmt.Println()
		fmt.Printf("⚠ %v NÃO vieram na resposta.\n", faltando)
		fmt.Println("  Isto é a armadilha do `filtroSaldoEstoque`: o default do Bling é 1 (só positivo),")
		fmt.Println("  então um produto ESGOTADO simplesmente some. Quem tratar ausência como 'não sei'")
		fmt.Println("  mantém o saldo velho para sempre e vende o que não tem.")
		fmt.Println("  Rode com --filtro 0 para ver os zerados e --filtro 2 para os negativos.")
	}
	fmt.Println()
	fmt.Println("VIRTUAL aqui é descrito no spec como 'DESCONSIDERANDO produtos reservados' — o")
	fmt.Println("oposto do que o MESMO campo diz em GET /produtos. Se os dois números divergirem")
	fmt.Println("para o mesmo produto, está respondida a pergunta T1 do plano.")
	fmt.Printf("\n%d requisição(ões) gastas da cota da conta.\n", cli.Chamadas())
	return nil
}

func cmdDepositos(ctx context.Context, cfg *config.Config, lg *audit.Log) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	ds, err := cli.Depositos(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d depósito(s)\n\n", len(ds))
	fmt.Printf("%-10s %-34s %8s %8s %s\n", "ID", "DESCRIÇÃO", "PADRÃO", "SITUAÇÃO", "DESCONSIDERA SALDO")
	for _, d := range ds {
		fmt.Printf("%-10d %-34s %8v %8d %v\n", d.ID, truncar(d.Descricao, 34), d.Padrao, d.Situacao, d.DesconsiderarSaldo)
	}
	fmt.Println()
	fmt.Println("Se houver depósito com 'desconsiderar saldo' ou que não deva vender (defeituosos,")
	fmt.Println("showroom, matéria-prima), o portão local NÃO pode somar o saldo total — é a dívida")
	fmt.Println("D5 do plano, e é aqui que ela se confirma ou se descarta.")
	return nil
}

// cmdBalanco DEFINE o saldo de um produto num depósito (POST /estoques, operacao=B).
//
// Existe só para o experimento controlado que responde duas perguntas de uma vez:
// se o webhook de estoque dispara de verdade, e se o saldoVirtualTotal diverge
// entre os dois endpoints quando há movimento. Passa pelos dois portões do
// Client.Write — sem BLING_ALLOW_WRITE e sem a conta na allowlist, não sai.
func cmdBalanco(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	pos := semFlags(args)
	if len(pos) < 3 {
		return fmt.Errorf("uso: bling-lab balanco <idProduto> <idDeposito> <quantidade> [--obs texto]")
	}
	ids, err := bling.IDsDeTexto(pos[:2])
	if err != nil {
		return err
	}
	qtd, err := strconv.ParseFloat(pos[2], 64)
	if err != nil {
		return fmt.Errorf("quantidade inválida: %q", pos[2])
	}

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	fmt.Printf("BALANÇO: produto %d, depósito %d → %.0f unidades\n", ids[0], ids[1], qtd)

	corpo, err := cli.LancarEstoque(ctx, bling.LancamentoEstoque{
		Produto:     bling.RefID{ID: ids[0]},
		Deposito:    bling.RefID{ID: ids[1]},
		Operacao:    bling.OperacaoBalanco,
		Quantidade:  qtd,
		Observacoes: valorStr(args, "--obs", "bling-lab: experimento reversível"),
	})
	if err != nil {
		return err
	}
	fmt.Println("✓ aceito pelo Bling")
	if len(corpo) > 0 {
		_ = imprimirJSONCru(corpo)
	}
	fmt.Println()
	fmt.Println("⚠ A resposta NÃO traz id de movimento, e não há GET por id nem listagem de")
	fmt.Println("  lançamentos. Se esta chamada tivesse dado timeout, não existiria pergunta")
	fmt.Println("  a fazer à API para saber se aplicou. É a razão de o plano manter escrita de")
	fmt.Println("  ESTOQUE fora do fluxo de produção.")
	return nil
}

// cmdEscritaCrua manda um verbo de escrita com corpo JSON arbitrário.
//
// É a ferramenta de exploração: descobrir o que a API aceita exige poder mandar
// exatamente o que se quer, inclusive coisas que um DTO tipado esconderia. Passa
// pelos MESMOS dois portões de qualquer escrita — a liberdade é de forma, não
// de destino.
func cmdEscritaCrua(ctx context.Context, cfg *config.Config, lg *audit.Log, metodo string, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	pos := semFlags(args)
	if len(pos) == 0 {
		return fmt.Errorf("uso: bling-lab %s <caminho> [json|@arquivo]", strings.ToLower(metodo))
	}
	caminho := pos[0]

	var corpo any
	if len(pos) > 1 {
		bruto := pos[1]
		if arquivo, ok := strings.CutPrefix(bruto, "@"); ok {
			b, err := os.ReadFile(arquivo)
			if err != nil {
				return err
			}
			bruto = string(b)
		}
		if err := json.Unmarshal([]byte(bruto), &corpo); err != nil {
			return fmt.Errorf("corpo não é JSON válido: %w", err)
		}
	}

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	r, err := cli.Write(ctx, metodo, caminho, corpo)
	if r != nil {
		fmt.Printf("HTTP %d  (%s)\n", r.Status, r.Duracao.Round(time.Millisecond))
	}
	if err != nil {
		return err
	}
	if r != nil && len(r.Body) > 0 {
		return imprimirJSONCru(r.Body)
	}
	fmt.Println("(sem corpo na resposta)")
	return nil
}

// cmdT1 roda a medição que decide a arquitetura de reserva.
func cmdT1(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	pos := semFlags(args)
	if len(pos) < 3 {
		return fmt.Errorf("uso: bling-lab t1 <idProduto> <idContato> <idFormaPagamento> [--unidades N]\n" +
			"  cria um pedido, relê o saldo, conclui, e EXCLUI o pedido")
	}
	ids, err := bling.IDsDeTexto(pos[:3])
	if err != nil {
		return err
	}
	unidades := valorInt(args, "--unidades", 3)

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	fmt.Printf("═══ T1 — o pedido em aberto reserva estoque? ═══\n\n")
	fmt.Printf("produto %d · %d unidade(s) · contato %d · forma de pagamento %d\n\n",
		ids[0], unidades, ids[1], ids[2])

	r, err := cli.MedirT1(ctx, strconv.FormatInt(ids[0], 10), unidades, ids[1], ids[2])
	if r != nil {
		fmt.Printf("%-26s %10s %10s\n", "", "ANTES", "DEPOIS")
		fmt.Printf("%-26s %10.0f %10.0f\n", "saldo FÍSICO", r.FisicoAntes, r.FisicoDepois)
		fmt.Printf("%-26s %10.0f %10.0f\n", "/estoques/saldos virtual", r.VirtualAntes, r.VirtualDepois)
		fmt.Printf("%-26s %10.0f %10.0f\n", "/produtos virtual", r.SaldoProdutosAntes, r.SaldoProdutosDepois)
		fmt.Println()
		if r.PedidoID != 0 {
			fmt.Printf("pedido de medição %d — %s\n", r.PedidoID,
				map[bool]string{true: "excluído ✓", false: "⚠ NÃO EXCLUÍDO, apague à mão"}[r.PedidoLimpo])
		}
		fmt.Println()
		if r.Conclusivo {
			fmt.Printf("VEREDITO: %s\n", r.Veredito)
		} else {
			fmt.Printf("SEM VEREDITO: %s\n", r.Veredito)
		}
		fmt.Printf("\n%d requisições gastas da cota.\n", r.Requisicoes)
	}
	return err
}

// ---------------------------------------------------------------- api cru

func cmdAPI(ctx context.Context, cfg *config.Config, lg *audit.Log, args []string) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	pos := semFlags(args)
	if len(pos) == 0 {
		return fmt.Errorf("uso: bling-lab api <caminho> [chave=valor ...]\n  ex: bling-lab api /situacoes/modulos")
	}
	caminho := pos[0]
	q := url.Values{}
	for _, kv := range pos[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("parâmetro inválido: %q (use chave=valor)", kv)
		}
		q.Add(k, v)
	}

	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)
	r, err := cli.Get(ctx, caminho, q)
	if r != nil {
		fmt.Printf("HTTP %d  (%s)\n", r.Status, r.Duracao.Round(time.Millisecond))
		if temFlag(args, "--headers") {
			imprimirHeaders(r)
		}
	}
	if err != nil {
		return err
	}
	return imprimirJSONCru(r.Body)
}

func imprimirHeaders(r *bling.Resposta) {
	var chaves []string
	for k := range r.Headers {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	fmt.Println("headers da resposta:")
	for _, k := range chaves {
		if strings.EqualFold(k, "set-cookie") {
			fmt.Printf("  %-32s <omitido>\n", k)
			continue
		}
		fmt.Printf("  %-32s %s\n", k, strings.Join(r.Headers[k], ", "))
	}
	fmt.Println()
}

// ---------------------------------------------------------------- probe

// cmdProbe é a rodada de medição A do plano: as perguntas que nenhum spec
// responde e que custam uma tarde se forem descobertas tarde.
func cmdProbe(ctx context.Context, cfg *config.Config, lg *audit.Log) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	cli := bling.New(cfg, oauth.NewClient(cfg, lg), lg)

	fmt.Println("═══ rodada de medição A ═══")
	fmt.Println()

	// 1. Identidade da conta.
	fmt.Println("1) IDENTIDADE DA CONTA — GET /empresas/me/dados-basicos")
	emp, err := cli.Empresa(ctx)
	if err != nil {
		return fmt.Errorf("não consegui ler a identidade da conta: %w", err)
	}
	fmt.Printf("   id=%q  (tipo: string, %d caracteres)\n", emp.ID, len(emp.ID))
	fmt.Printf("   nome=%q cnpj=%q\n", emp.Nome, emp.CNPJ)
	fmt.Println("   → candidato a chave de cota e ao companyId do webhook. Falta casar com um evento real.")
	fmt.Println()

	// 2. Headers de cota.
	fmt.Println("2) HEADERS DE COTA — a API devolve algum?")
	r, err := cli.Get(ctx, "/depositos", nil)
	if err != nil && r == nil {
		return err
	}
	var achados []string
	for k := range r.Headers {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "rate-limit") ||
			lk == "retry-after" || strings.Contains(lk, "quota") {
			achados = append(achados, k+": "+r.Headers.Get(k))
		}
	}
	if len(achados) == 0 {
		fmt.Println("   NENHUM. Confirmado contra esta conta.")
		fmt.Println("   → o limitador do Bling tem de ser PREDITIVO puro. O AdaptiveLimiter do LiveCart,")
		fmt.Println("     que depende de header, sairia SEM FREIO NENHUM contra o Bling.")
	} else {
		fmt.Println("   ACHOU:")
		for _, a := range achados {
			fmt.Println("     " + a)
		}
		fmt.Println("   → REVISAR O PLANO: com header, dá para reconciliar a cota e o item encolhe.")
	}
	fmt.Printf("   (headers presentes: %d — todos gravados em %s)\n", len(r.Headers), lg.Path())
	fmt.Println()

	// 3. Depósitos — decide se o portão pode somar o saldo total.
	fmt.Println("3) DEPÓSITOS — o portão local pode somar o saldo total?")
	ds, err := cli.Depositos(ctx)
	if err != nil {
		fmt.Printf("   falhou: %v\n", err)
	} else {
		fmt.Printf("   %d depósito(s)\n", len(ds))
		suspeitos := 0
		for _, d := range ds {
			marca := " "
			if d.DesconsiderarSaldo {
				marca = "!"
				suspeitos++
			}
			fmt.Printf("   %s id=%-8d padrao=%-5v desconsiderarSaldo=%-5v  %s\n", marca, d.ID, d.Padrao, d.DesconsiderarSaldo, d.Descricao)
		}
		if suspeitos > 0 || len(ds) > 1 {
			fmt.Println("   → mais de um depósito (ou algum marcado para desconsiderar): a dívida D5 é REAL nesta conta.")
		} else {
			fmt.Println("   → depósito único e sem exclusão: somar o total é seguro NESTA conta.")
		}
	}
	fmt.Println()

	// 4. Catálogo — o formato dos produtos que existem.
	fmt.Println("4) CATÁLOGO — o que existe nesta conta")
	prods, err := cli.ListarProdutos(ctx, bling.ListarProdutosParams{Limite: 100, Criterio: 5, Tipo: "T"})
	if err != nil {
		fmt.Printf("   falhou: %v\n", err)
	} else {
		porFormato := map[string]int{}
		for _, p := range prods {
			porFormato[p.Formato]++
		}
		fmt.Printf("   %d produto(s) com criterio=5 (todos), tipo=T\n", len(prods))
		fmt.Printf("   por formato: S(simples)=%d  V(com variações)=%d  E(composição)=%d\n",
			porFormato["S"], porFormato["V"], porFormato["E"])
		for _, p := range prods {
			fmt.Printf("     id=%-10d sku=%-12s saldo(/produtos)=%-8.0f %s\n", p.ID, truncar(p.Codigo, 12), p.Estoque.SaldoVirtualTotal, truncar(p.Nome, 40))
		}

		// 5. A contradição do saldo virtual, medida.
		if len(prods) > 0 {
			fmt.Println()
			fmt.Println("5) A CONTRADIÇÃO DO saldoVirtualTotal — o mesmo campo, descrito ao contrário")
			var ids []int64
			for _, p := range prods {
				ids = append(ids, p.ID)
			}
			if len(ids) > 20 {
				ids = ids[:20]
			}
			// filtro 1 (positivo) é o default do Bling; pedimos explícito para o
			// experimento ser reproduzível.
			saldos, err := cli.Saldos(ctx, ids, 1)
			if err != nil {
				fmt.Printf("   falhou: %v\n", err)
			} else {
				porID := map[int64]float64{}
				for _, s := range saldos {
					porID[s.Produto.ID] = s.SaldoVirtualTotal
				}
				divergiu := 0
				fmt.Printf("   %-12s %18s %18s\n", "ID", "/produtos", "/estoques/saldos")
				for _, p := range prods {
					v, ok := porID[p.ID]
					if !ok {
						fmt.Printf("   %-12d %18.2f %18s\n", p.ID, p.Estoque.SaldoVirtualTotal, "AUSENTE(filtro=1)")
						continue
					}
					flag := ""
					if v != p.Estoque.SaldoVirtualTotal {
						flag = "  ← DIVERGE"
						divergiu++
					}
					fmt.Printf("   %-12d %18.2f %18.2f%s\n", p.ID, p.Estoque.SaldoVirtualTotal, v, flag)
				}
				fmt.Println()
				if divergiu > 0 {
					fmt.Printf("   → %d produto(s) DIVERGEM. Os dois campos NÃO são intercambiáveis, e a escolha\n", divergiu)
					fmt.Println("     de qual alimenta o portão local é decisão de arquitetura, não detalhe.")
				} else {
					fmt.Println("   → nenhuma divergência AGORA. Mas sem pedido em aberto nesta conta, o teste")
					fmt.Println("     é inconclusivo: a diferença só aparece quando há reserva. Repetir com um")
					fmt.Println("     pedido de venda aberto é a medição T1 de verdade.")
				}
			}
		}
	}

	fmt.Println()
	fmt.Printf("═══ fim — %d requisições gastas da cota desta conta ═══\n", cli.Chamadas())
	fmt.Printf("Tudo gravado em %s\n", lg.Path())
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
	case "serve", "":
		srv := hooks.NewServer(store, lg, cfg.HooksPort, cfg.ClientSecret,
			valorStr(args, "--forward", ""), temFlag(args, "--estrito"))
		return srv.Serve(ctx)

	case "list":
		evs, err := store.Listar()
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			fmt.Printf("nenhum webhook gravado em %s\n", store.Dir())
			return nil
		}
		for _, e := range evs {
			fmt.Printf("%-28s %s  [%s]  %s\n", e.ID, e.ReceivedAt.Local().Format("2006-01-02 15:04:05"), e.Assinatura, e.Resumo())
		}
		return nil

	case "show":
		pos := semFlags(args[1:])
		if len(pos) == 0 {
			return fmt.Errorf("uso: bling-lab hooks show <id>")
		}
		e, err := store.Carregar(pos[0])
		if err != nil {
			return err
		}
		return imprimirJSON(e)

	case "replay":
		pos := semFlags(args[1:])
		if len(pos) < 2 {
			return fmt.Errorf("uso: bling-lab hooks replay <id> <url> [--vezes N]")
		}
		e, err := store.Carregar(pos[0])
		if err != nil {
			return err
		}
		return hooks.Reenviar(ctx, e, pos[1], cfg.ClientSecret, valorInt(args, "--vezes", 1))

	default:
		return fmt.Errorf("subcomando de hooks desconhecido: %q (serve|list|show|replay)", sub)
	}
}

// ---------------------------------------------------------------- audit / config

func cmdAudit(cfg *config.Config, args []string) error {
	b, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("nada auditado ainda (%s)\n", cfg.AuditPath())
			return nil
		}
		return err
	}
	linhas := strings.Split(strings.TrimSpace(string(b)), "\n")
	n := valorInt(args, "--n", 20)
	if n < len(linhas) {
		linhas = linhas[len(linhas)-n:]
	}
	for _, l := range linhas {
		var e audit.Entry
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		status := ""
		if e.Status > 0 {
			status = strconv.Itoa(e.Status)
		}
		fmt.Printf("%s  %-8s %-6s %-4s %6dms  %s %s\n",
			e.At.Local().Format("15:04:05"), e.Kind, e.Method, status, e.DurationMS, e.URL, e.Error)
	}
	fmt.Printf("\n(%s)\n", cfg.AuditPath())
	return nil
}

func cmdConfig(cfg *config.Config) error {
	mask := func(s string) string {
		if s == "" {
			return "(vazio)"
		}
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "…" + s[len(s)-4:]
	}
	fmt.Printf("raiz               %s\n", cfg.Root())
	fmt.Printf("client_id          %s\n", mask(cfg.ClientID))
	fmt.Printf("client_secret      %s\n", mask(cfg.ClientSecret))
	fmt.Printf("redirect cadastrado %s\n", cfg.RedirectURI)
	fmt.Printf("  é local?         %v  (escuta em 127.0.0.1:%d%s)\n", cfg.RedirectIsLocal(), cfg.ListenPort(), cfg.CallbackPath())
	fmt.Printf("authorize          %s\n", cfg.AuthURL)
	fmt.Printf("token              %s\n", cfg.TokenURL)
	fmt.Printf("api base           %s\n", cfg.APIBase)
	fmt.Printf("freio              %.2f req/s (teto do Bling: 3/s POR CONTA, somando todos os apps)\n", cfg.RateLimitRPS)
	fmt.Printf("porta dos webhooks %d\n", cfg.HooksPort)
	fmt.Printf("escrita liberada   %v", cfg.AllowWrite)
	if cfg.AllowWrite {
		fmt.Printf("  allowlist=%v", cfg.AllowedCompanyID)
	}
	fmt.Println()
	fmt.Printf("estado             %s\n", cfg.StateDir)
	return nil
}

// ---------------------------------------------------------------- util

func usage() {
	fmt.Print(`bling-lab — exploração da API v3 do Bling contra uma conta REAL (não existe sandbox)

  auth login [--no-browser]     autoriza e guarda o token
  auth status                   mostra validade e se o refresh rotaciona
  auth refresh                  renova e MEDE se o refresh token rotacionou
  auth revoke --sim             revoga o refresh token desta conta

  empresa                       identidade da conta (candidato a companyId)
  produtos [--criterio N] [--tipo T] [--limite N] [--pagina N] [--json]
  produto <id>                  o JSON cru de um produto
  variacoes <idPai>             produto pai + variações
  saldos <id...> [--filtro 0|1|2] [--json]      saldo EM LOTE
  depositos                     depósitos da conta
  balanco <idProd> <idDep> <qtd>  DEFINE o saldo (POST /estoques, operacao=B) — ESCRITA
  post|put|patch|delete <caminho> [json|@arquivo]   escrita crua — ESCRITA

  api <caminho> [k=v...] [--headers]            GET em qualquer endpoint
  probe                         rodada de medição A (identidade, cota, saldo)

  hooks serve [--forward URL] [--estrito]       ponte de webhooks com HMAC
  hooks list | show <id> | replay <id> <url> [--vezes N]

  audit [--n N]                 últimas linhas do log
  config                        o que está configurado agora

Escrita é BLOQUEADA por padrão. Liberar exige BLING_ALLOW_WRITE=true E
BLING_ALLOWED_COMPANY_ID com o id da conta — dois portões, porque um engano
aqui escreve no ERP de um lojista de verdade.
`)
}

func temFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

func valorStr(args []string, f, def string) string {
	for i, a := range args {
		if a == f && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, f+"="); ok {
			return v
		}
	}
	return def
}

func valorInt(args []string, f string, def int) int {
	s := valorStr(args, f, "")
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// semFlags devolve os argumentos posicionais, pulando flags e seus valores.
func semFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++ // consome o valor da flag
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func imprimirJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func imprimirJSONCru(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		fmt.Println(string(b))
		return nil
	}
	return imprimirJSON(v)
}

func truncar(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
