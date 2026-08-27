package erp

// O módulo de Reserva de Estoque do Tiny, e por que ele não é uma opção nossa.
//
// O LiveCart lê SEMPRE o saldo `disponivel` — nunca o físico. O físico conta
// peça que já tem dono (orçamento salvo, pedido em aberto, venda de outro
// canal), e vender em cima dele é vender o que não existe. Isso deixou de ser
// configuração: não há botão, e não deveria haver, porque não existe resposta
// certa "para algumas lojas".
//
// Quem decide se a peça fica reservada é o próprio Tiny. Com o módulo de
// Reserva ATIVO, criar o pedido de venda move `reservado` e derruba
// `disponivel` — a peça sai da prateleira no instante do comentário. Sem o
// módulo, o pedido nasce igual, mas nada sai da prateleira até o faturamento.
//
// Então a escolha existe, e ela é do lojista — só que o lugar dela é a conta do
// Tiny, não a nossa tela:
//
//	quer que a live segure a peça?      →  ative a Reserva de Estoque no Tiny
//	prefere vender até o faturamento?   →  não ative
//
// ═══ POR QUE NÃO DÁ PARA CONFERIR SOZINHO ═══
//
// `GET /depositos` traz `possuiReserva`, que responderia isso de forma direta —
// e devolve 403 mesmo numa conta com o módulo ativo (medido em 25, 26 e
// 27/08/2026 na conta ADABYTE). Não há outro endpoint na v3 que declare o
// módulo.
//
// O que sobra é a evidência indireta: `GET /estoque/{id}` devolve os três
// saldos, e `reservado > 0` em qualquer produto PROVA que o módulo está ativo.
// O contrário não vale — uma loja com o módulo ativo e nada vendido também
// mostra tudo zerado. Por isso esta checagem tem TRÊS respostas, e a do meio é
// "não sei": dizer "desativado" com base em ausência de prova mandaria o
// lojista mexer numa configuração que talvez já esteja certa.

import (
	"context"
	"fmt"

	"livecart/apps/api/internal/integration/providers"
)

// ReservaStatus é o que conseguimos afirmar sobre o módulo de Reserva.
type ReservaStatus string

const (
	// ReservaConfirmada: vimos `reservado > 0`. Prova positiva.
	ReservaConfirmada ReservaStatus = "confirmada"
	// ReservaIndeterminada: nenhum produto com reserva no momento. Pode ser
	// módulo desligado, pode ser loja parada. Não dá para distinguir.
	ReservaIndeterminada ReservaStatus = "indeterminada"
	// ReservaNaoVerificada: não deu para ler o estoque (sem produtos ligados ao
	// ERP, ou o Tiny não respondeu).
	ReservaNaoVerificada ReservaStatus = "nao_verificada"
)

// ReservaCheck é o retrato da checagem, com o material que a sustenta.
type ReservaCheck struct {
	Status ReservaStatus
	// Amostrados é quantos produtos foram lidos.
	Amostrados int
	// ComReserva é em quantos deles `reservado > 0`.
	ComReserva int
	// Exemplo é o nome de um produto com reserva, para o lojista reconhecer a
	// evidência em vez de ter de confiar no número.
	Exemplo string
	// Motivo explica um `nao_verificada`.
	Motivo string
}

// amostraDaReserva é quantos produtos a checagem lê. Pequena de propósito: o
// teto da conta é 30 escritas/min e as leituras dividem a mesma cota com a
// live. Provar o módulo não vale atrasar uma venda.
const amostraDaReserva = 5

// VerificarReserva procura evidência de que o módulo de Reserva está ativo.
//
// Nunca devolve "desativado": a ausência de reserva não prova a ausência do
// módulo, e afirmar o contrário mandaria o lojista mexer no que já está certo.
func (s *Service) VerificarReserva(ctx context.Context, storeID string) (*ReservaCheck, error) {
	rel := &ReservaCheck{Status: ReservaNaoVerificada}

	produtos, err := s.repo.ListERPLinkedProductsSample(ctx, storeID, amostraDaReserva)
	if err != nil {
		rel.Motivo = "não foi possível listar os produtos ligados ao ERP"
		return rel, err
	}
	if len(produtos) == 0 {
		rel.Motivo = "nenhum produto ligado ao Tiny ainda — importe produtos para poder verificar"
		return rel, nil
	}

	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		rel.Motivo = "integração com o Tiny indisponível"
		return rel, err
	}
	leitor, ok := erpProvider.(interface {
		GetProductStockDetail(ctx context.Context, externalID string) (providers.ERPStockDetail, error)
	})
	if !ok {
		rel.Motivo = "este ERP não expõe o saldo reservado"
		return rel, nil
	}

	var lidos int
	for _, p := range produtos {
		detalhe, err := leitor.GetProductStockDetail(ctx, p.ExternalID)
		if err != nil {
			continue // um produto que não responde não invalida a amostra
		}
		lidos++
		if detalhe.Reserved > 0 {
			rel.ComReserva++
			if rel.Exemplo == "" {
				rel.Exemplo = p.Name
			}
		}
	}
	rel.Amostrados = lidos

	switch {
	case lidos == 0:
		rel.Motivo = "o Tiny não respondeu à leitura de estoque"
	case rel.ComReserva > 0:
		rel.Status = ReservaConfirmada
	default:
		rel.Status = ReservaIndeterminada
	}
	return rel, nil
}

// String é o que aparece no log.
func (c *ReservaCheck) String() string {
	return fmt.Sprintf("reserva=%s amostrados=%d com_reserva=%d", c.Status, c.Amostrados, c.ComReserva)
}
