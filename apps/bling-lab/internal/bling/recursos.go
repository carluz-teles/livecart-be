package bling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- empresa

// Empresa é a identidade da conta conectada.
//
// ATENÇÃO ao tipo de ID: é STRING no spec (exemplo de 32 hex), não inteiro.
// É o candidato natural a chave de cota e ao `companyId` que o webhook manda —
// mas que os dois são o MESMO valor ainda não está provado. `bling-lab probe`
// e `hooks serve` existem para pôr os dois lado a lado.
type Empresa struct {
	ID           string `json:"id"`
	Nome         string `json:"nome"`
	CNPJ         string `json:"cnpj"`
	Email        string `json:"email"`
	DataContrato string `json:"dataContrato"`
}

func (c *Client) Empresa(ctx context.Context) (*Empresa, error) {
	// GET /empresas puro NÃO existe — o único path da tag Empresas é este.
	r, err := c.Get(ctx, "/empresas/me/dados-basicos", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data Empresa `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, fmt.Errorf("resposta de /empresas/me/dados-basicos não é o envelope esperado: %w", err)
	}
	return &env.Data, nil
}

// ---------------------------------------------------------------- produtos

type Produto struct {
	ID             int64   `json:"id"`
	IDProdutoPai   int64   `json:"idProdutoPai"`
	Nome           string  `json:"nome"`
	Codigo         string  `json:"codigo"`
	Preco          float64 `json:"preco"`
	PrecoCusto     float64 `json:"precoCusto"`
	Tipo           string  `json:"tipo"`     // S serviço, P produto, N serviço 06/21/22
	Situacao       string  `json:"situacao"` // A ativo, I inativo
	Formato        string  `json:"formato"`  // S simples, V com variações, E com composição
	DescricaoCurta string  `json:"descricaoCurta"`
	ImagemURL      string  `json:"imagemURL"`

	Estoque struct {
		// ⚠ MESMO NOME, DESCRIÇÃO OPOSTA à de /estoques/saldos.
		// Aqui o spec diz "considerando a reserva de estoque";
		// lá diz "desconsiderando produtos reservados".
		// Enquanto ninguém medir, os dois números não são intercambiáveis.
		SaldoVirtualTotal float64 `json:"saldoVirtualTotal"`
	} `json:"estoque"`
}

// ListarProdutosParams espelha os filtros que a listagem aceita. Todos os
// campos são explicitados nas chamadas do laboratório de propósito: confiar no
// default de `criterio` (1 = "últimos incluídos") é como se perde produto sem
// perceber que se perdeu.
type ListarProdutosParams struct {
	Pagina   int
	Limite   int
	Criterio int    // 1 últimos incluídos, 2 ativos, 3 inativos, 4 excluídos, 5 todos
	Tipo     string // T todos, P produtos, S serviços, E composições, PS simples, C com variações, V variações
	Nome     string
	Codigos  []string
	IDs      []int64
	// DataAlteracaoInicial habilita o sync incremental.
	DataAlteracaoInicial string
}

func (p ListarProdutosParams) query() url.Values {
	q := url.Values{}
	if p.Pagina > 0 {
		q.Set("pagina", strconv.Itoa(p.Pagina))
	}
	if p.Limite > 0 {
		q.Set("limite", strconv.Itoa(p.Limite))
	}
	if p.Criterio > 0 {
		q.Set("criterio", strconv.Itoa(p.Criterio))
	}
	if p.Tipo != "" {
		q.Set("tipo", p.Tipo)
	}
	if p.Nome != "" {
		q.Set("nome", p.Nome)
	}
	if p.DataAlteracaoInicial != "" {
		q.Set("dataAlteracaoInicial", p.DataAlteracaoInicial)
	}
	for _, c := range p.Codigos {
		q.Add("codigos[]", c)
	}
	for _, id := range p.IDs {
		q.Add("idsProdutos[]", strconv.FormatInt(id, 10))
	}
	return q
}

func (c *Client) ListarProdutos(ctx context.Context, p ListarProdutosParams) ([]Produto, error) {
	r, err := c.Get(ctx, "/produtos", p.query())
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Produto `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, fmt.Errorf("resposta de /produtos não é o envelope esperado: %w", err)
	}
	return env.Data, nil
}

// ProdutoCru devolve o JSON íntegro de um produto. Cru de propósito: o
// laboratório existe para descobrir o que o Bling manda, e um struct tipado
// esconde exatamente os campos que ainda não conhecemos.
func (c *Client) ProdutoCru(ctx context.Context, id int64) (json.RawMessage, error) {
	r, err := c.Get(ctx, "/produtos/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// VariacoesCru devolve o produto pai com as variações.
func (c *Client) VariacoesCru(ctx context.Context, idPai int64) (json.RawMessage, error) {
	r, err := c.Get(ctx, "/produtos/variacoes/"+strconv.FormatInt(idPai, 10), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// ---------------------------------------------------------------- estoque

// SaldoProduto é uma linha de GET /estoques/saldos.
type SaldoProduto struct {
	Produto struct {
		ID     int64  `json:"id"`
		Codigo string `json:"codigo"`
	} `json:"produto"`
	SaldoFisicoTotal float64 `json:"saldoFisicoTotal"`
	// ⚠ Aqui o spec diz "DESCONSIDERANDO produtos reservados" — o oposto do que
	// diz em GET /produtos para o campo de mesmo nome. Ver Produto.Estoque.
	SaldoVirtualTotal float64 `json:"saldoVirtualTotal"`
	Depositos         []struct {
		ID           int64   `json:"id"`
		SaldoFisico  float64 `json:"saldoFisico"`
		SaldoVirtual float64 `json:"saldoVirtual"`
	} `json:"depositos"`
}

// Saldos lê o saldo de VÁRIOS produtos numa requisição — a vantagem real do
// Bling sobre o Tiny, onde é 1 GET por produto e foi a fonte dos 429.
//
// filtroSaldo: 0 zerado, 1 positivo, 2 negativo. O default do Bling é 1, e é
// uma armadilha: um produto esgotado simplesmente NÃO VEM na resposta, e quem
// interpretar ausência como "não sei" mantém o saldo velho para sempre. O
// laboratório manda o filtro sempre explícito.
func (c *Client) Saldos(ctx context.Context, ids []int64, filtroSaldo int) ([]SaldoProduto, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("nenhum id de produto informado (idsProdutos[] é obrigatório)")
	}
	q := url.Values{}
	for _, id := range ids {
		q.Add("idsProdutos[]", strconv.FormatInt(id, 10))
	}
	if filtroSaldo >= 0 {
		q.Set("filtroSaldoEstoque", strconv.Itoa(filtroSaldo))
	}
	r, err := c.Get(ctx, "/estoques/saldos", q)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []SaldoProduto `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, fmt.Errorf("resposta de /estoques/saldos não é o envelope esperado: %w", err)
	}
	return env.Data, nil
}

type Deposito struct {
	ID                 int64  `json:"id"`
	Descricao          string `json:"descricao"`
	Situacao           int    `json:"situacao"`
	Padrao             bool   `json:"padrao"`
	DesconsiderarSaldo bool   `json:"desconsiderarSaldo"`
}

func (c *Client) Depositos(ctx context.Context) ([]Deposito, error) {
	r, err := c.Get(ctx, "/depositos", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Deposito `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return nil, fmt.Errorf("resposta de /depositos não é o envelope esperado: %w", err)
	}
	return env.Data, nil
}

// ---------------------------------------------------------------- util

// IDsDeTexto converte "123,456 789" numa lista de ids.
func IDsDeTexto(args []string) ([]int64, error) {
	var out []int64
	for _, a := range args {
		for _, campo := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.ParseInt(strings.TrimSpace(campo), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("id de produto inválido: %q", campo)
			}
			out = append(out, n)
		}
	}
	return out, nil
}
