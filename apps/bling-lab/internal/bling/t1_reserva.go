package bling

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// T1 — a medição que decide a arquitetura de reserva.
//
// A pergunta: um pedido de venda EM ABERTO tira a peça do saldo disponível?
//
// A resposta depende de uma configuração da CONTA que o LiveCart não consegue
// ler nem ligar por API (verificado: /configuracoes, /preferencias,
// /empresas/me/configuracoes e /estoques/configuracoes respondem 404; os únicos
// endpoints de configuração das 162 rotas são GET|PUT /nfse/configuracoes).
// Doc oficial: perfil da empresa → Todas as configurações → Suprimentos →
// Estoque → "Considerar situações de vendas para obter o saldo atual (Reserva
// de estoque)", e só o usuário administrador consegue.
//
// Por isso o experimento é EMPÍRICO: cria um pedido, relê o saldo, e conclui
// pela diferença. É reversível por construção — o pedido é excluído no fim,
// inclusive quando a medição falha no meio.

type ResultadoT1 struct {
	ProdutoID string
	Unidades  int

	FisicoAntes   float64
	VirtualAntes  float64
	FisicoDepois  float64
	VirtualDepois float64

	// SaldoProdutosAntes/Depois é o campo de MESMO NOME em GET /produtos, que o
	// spec descreve de forma OPOSTA ao de /estoques/saldos. Lê-se os dois para
	// descobrir qual deles realmente reflete a reserva.
	SaldoProdutosAntes  float64
	SaldoProdutosDepois float64

	PedidoID    int64
	Reserva     bool
	Conclusivo  bool
	Veredito    string
	Requisicoes int
	PedidoLimpo bool
}

// MedirT1 roda o experimento inteiro e devolve o veredito.
//
// contatoID e formaPagamentoID vêm da conta; unidades é quanto o pedido pede.
func (c *Client) MedirT1(ctx context.Context, produtoID string, unidades int, contatoID, formaPagamentoID int64) (*ResultadoT1, error) {
	r := &ResultadoT1{ProdutoID: produtoID, Unidades: unidades}

	ler := func() (fisico, virtual, produtos float64, err error) {
		saldos, err := c.Saldos(ctx, []int64{parseID(produtoID)}, -1)
		if err != nil {
			return 0, 0, 0, err
		}
		if len(saldos) == 0 {
			return 0, 0, 0, fmt.Errorf("o produto %s não veio na resposta de saldo", produtoID)
		}
		prods, err := c.ListarProdutos(ctx, ListarProdutosParams{
			IDs: []int64{parseID(produtoID)}, Limite: 1, Criterio: 5, Tipo: "T",
		})
		if err != nil {
			return 0, 0, 0, err
		}
		var pv float64
		if len(prods) > 0 {
			pv = prods[0].Estoque.SaldoVirtualTotal
		}
		return saldos[0].SaldoFisicoTotal, saldos[0].SaldoVirtualTotal, pv, nil
	}

	var err error
	r.FisicoAntes, r.VirtualAntes, r.SaldoProdutosAntes, err = ler()
	if err != nil {
		return r, fmt.Errorf("lendo o saldo ANTES: %w", err)
	}

	agora := time.Now().In(saoPaulo)
	pedido := map[string]any{
		"contato":      map[string]any{"id": contatoID},
		"data":         agora.Format("2006-01-02"),
		"dataSaida":    agora.Format("2006-01-02"),
		"dataPrevista": agora.AddDate(0, 0, 7).Format("2006-01-02"),
		"numeroLoja":   "lc-t1-" + agora.Format("150405"),
		"observacoes":  "bling-lab T1: medicao de reserva. Excluido automaticamente.",
		"itens": []any{map[string]any{
			"produto":    map[string]any{"id": parseID(produtoID)},
			"quantidade": unidades,
			"valor":      1,
			"descricao":  "T1 reserva",
		}},
		"parcelas": []any{map[string]any{
			"dataVencimento": agora.AddDate(0, 0, 7).Format("2006-01-02"),
			"valor":          unidades,
			"formaPagamento": map[string]any{"id": formaPagamentoID},
		}},
	}

	resp, err := c.Write(ctx, "POST", "/pedidos/vendas", pedido)
	if err != nil {
		return r, fmt.Errorf("criando o pedido de medição: %w", err)
	}
	var env struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(resp.Body, &env)
	r.PedidoID = env.Data.ID

	// A limpeza é garantida mesmo se a leitura seguinte falhar: deixar um pedido
	// de teste vivo na conta do lojista seria pior do que não medir.
	defer func() {
		if r.PedidoID == 0 {
			return
		}
		limpar, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := c.Write(limpar, "DELETE", fmt.Sprintf("/pedidos/vendas/%d", r.PedidoID), nil); err == nil {
			r.PedidoLimpo = true
		}
	}()

	// O Bling processa a reserva de forma assíncrona ao POST. Sem esta espera a
	// medição leria o saldo antes de o efeito aparecer e concluiria "não
	// reserva" numa conta que reserva — um falso negativo que mandaria o
	// lojista para o modo errado.
	time.Sleep(8 * time.Second)

	r.FisicoDepois, r.VirtualDepois, r.SaldoProdutosDepois, err = ler()
	if err != nil {
		return r, fmt.Errorf("lendo o saldo DEPOIS: %w", err)
	}

	r.avaliar()
	r.Requisicoes = c.Chamadas()
	return r, nil
}

func (r *ResultadoT1) avaliar() {
	caiuVirtual := r.VirtualAntes - r.VirtualDepois
	caiuProdutos := r.SaldoProdutosAntes - r.SaldoProdutosDepois
	caiuFisico := r.FisicoAntes - r.FisicoDepois

	switch {
	case caiuFisico > 0:
		// O físico NÃO deveria se mexer com pedido em aberto — isso é baixa de
		// estoque, não reserva. Se acontecer, a conta tem alguma automação de
		// lançamento na criação do pedido, e o desenho precisa saber disso.
		r.Conclusivo = true
		r.Reserva = true
		r.Veredito = fmt.Sprintf("⚠ o saldo FÍSICO caiu %.0f — isso é BAIXA, não reserva. "+
			"A conta tem lançamento automático de estoque na criação do pedido", caiuFisico)

	case caiuVirtual >= float64(r.Unidades) || caiuProdutos >= float64(r.Unidades):
		r.Conclusivo = true
		r.Reserva = true
		r.Veredito = fmt.Sprintf("RESERVA LIGADA: %d unidade(s) em pedido aberto derrubaram "+
			"o disponível (/estoques/saldos: -%.0f, /produtos: -%.0f) sem mexer no físico",
			r.Unidades, caiuVirtual, caiuProdutos)

	case caiuVirtual == 0 && caiuProdutos == 0:
		r.Conclusivo = true
		r.Reserva = false
		r.Veredito = fmt.Sprintf("RESERVA DESLIGADA: %d unidade(s) em pedido aberto e NENHUM "+
			"dos dois saldos se mexeu (%.0f → %.0f). A conta não considera pedido em aberto "+
			"para o saldo atual", r.Unidades, r.VirtualAntes, r.VirtualDepois)

	default:
		r.Conclusivo = false
		r.Veredito = fmt.Sprintf("INCONCLUSIVO: o disponível caiu %.0f para um pedido de %d "+
			"unidade(s) — reserva parcial, ou alguém mexeu no estoque durante a medição",
			caiuVirtual, r.Unidades)
	}
}

var saoPaulo = time.FixedZone("America/Sao_Paulo", -3*60*60)

func parseID(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
