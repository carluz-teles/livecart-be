package bling

import (
	"context"
	"encoding/json"
	"fmt"
)

// LancamentoEstoque é o corpo de POST /estoques.
//
// `operacao` é o campo que decide o estrago: `B` (balanço) DEFINE o saldo,
// `E` (entrada) e `S` (saída) o movimentam em relação ao que existe. Para um
// experimento reversível, balanço é o único seguro — entrada e saída dependem
// de o saldo lido não ter mudado no meio, e o Bling não tem leitura-com-versão.
type LancamentoEstoque struct {
	Produto     RefID   `json:"produto"`
	Deposito    RefID   `json:"deposito"`
	Operacao    string  `json:"operacao"`
	Quantidade  float64 `json:"quantidade"`
	Preco       float64 `json:"preco,omitempty"`
	Custo       float64 `json:"custo,omitempty"`
	Observacoes string  `json:"observacoes,omitempty"`
}

type RefID struct {
	ID int64 `json:"id"`
}

const (
	OperacaoBalanco = "B"
	OperacaoEntrada = "E"
	OperacaoSaida   = "S"
)

// LancarEstoque escreve um lançamento de estoque. Passa pelos DOIS portões do
// Client.Write (flag de ambiente + allowlist da conta conferida contra a
// identidade real), então não há caminho para ele acertar a conta errada.
//
// ⚠ A resposta NÃO traz id de movimento, e a tag Estoques não tem GET por id
// nem listagem de lançamentos. Um timeout aqui é IRRESOLÚVEL: não existe
// pergunta que se possa fazer à API para saber se aplicou. Só a releitura do
// SALDO diz alguma coisa, e ela é ambígua se houver concorrência. É por isso
// que o plano mantém esta operação FORA do fluxo de produção.
func (c *Client) LancarEstoque(ctx context.Context, l LancamentoEstoque) (json.RawMessage, error) {
	if l.Operacao != OperacaoBalanco && l.Operacao != OperacaoEntrada && l.Operacao != OperacaoSaida {
		return nil, fmt.Errorf("operacao inválida: %q (use B, E ou S)", l.Operacao)
	}
	if l.Produto.ID == 0 || l.Deposito.ID == 0 {
		return nil, fmt.Errorf("produto.id e deposito.id são obrigatórios")
	}
	r, err := c.Write(ctx, "POST", "/estoques", l)
	if r != nil && len(r.Body) > 0 {
		return r.Body, err
	}
	return nil, err
}
