package erp

// Reconciliação de estoque: comparar o que acreditamos com o que o ERP diz.
//
// Não existia detecção nenhuma. O desvio de 12/08/2026 — uma unidade inventada
// no Gabinete Gamer e uma perdida no Perfume Cebolinha — só apareceu porque o
// lojista conferiu o Tiny na mão, horas depois; e para explicá-lo foi preciso
// reconstruir o razão inteiro a partir de integration_logs, movimento a
// movimento.
//
// As correções daquele incidente fecharam as janelas conhecidas. Isto aqui é a
// resposta à pergunta seguinte, que é a que importa: como sabemos da PRÓXIMA?
// Nenhum sistema que fala com um ERP externo por deltas fica correto para
// sempre — rede cai, processo morre entre a chamada e o commit, o lojista mexe
// no estoque por fora. O que dá para garantir é não descobrir tarde.
//
// Só DETECTA, nunca corrige. Corrigir automaticamente exige saber a causa: um
// saldo menor pode ser unidade nossa perdida ou venda que o lojista fez no
// balcão, e as duas pedem ações opostas. Escrever no ERP com base num palpite
// dobra o estrago que a reconciliação existe para achar.

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// StockPosition é o que o sistema acredita sobre um produto.
type StockPosition struct {
	ProductID  string
	Name       string
	ExternalID string
	LocalStock int
}

// StockDivergence é um produto cujo saldo no ERP não bate com o esperado.
type StockDivergence struct {
	ProductID  string
	Name       string
	ExternalID string
	// Expected é o contador local — que, no modelo de pedido-como-reserva, é
	// exatamente o que o ERP deveria reportar como disponível.
	Expected int
	// Actual é o saldo DISPONÍVEL que o ERP respondeu.
	Actual int
	// Delta é Actual − Expected. Positivo significa unidade a mais no ERP do que
	// deveria (foi o Gabinete, +1); negativo, unidade a menos (o Perfume, −1).
	Delta int
}

// ReconciliationReport é o resultado de uma varredura.
type ReconciliationReport struct {
	StoreID     string
	Checked     int
	Skipped     int
	Divergences []StockDivergence
}

// StockPositionReader devolve o que acreditamos, por produto ligado ao ERP.
type StockPositionReader interface {
	ListStockPositionsForReconciliation(ctx context.Context, storeID, externalSource string) ([]StockPosition, error)
}

// ReconcileStockAgainstERP compara, produto a produto, o saldo que o ERP reporta
// com o que deveria estar lá segundo o nosso estado.
//
// A conta é uma igualdade simples: `LocalStock == disponivel`. Ela ficou simples
// porque os dois lados passaram a medir a mesma coisa — o carrinho aberto
// desconta do contador local aqui e, como virou pedido de venda, desconta do
// disponível lá. Antes era `local − reservas ativas`, e cada termo dessa
// subtração era uma oportunidade de errar.
//
// Uma varredura só é confiável em repouso. Durante uma live existe uma janela de
// alguns segundos entre o item entrar no carrinho e o pedido refletir no ERP, e a
// foto sai borrada — divergências transitórias que se resolvem sozinhas. Chame
// com a loja quieta, ou trate os resultados como suspeita e não como veredito.
func ReconcileStockAgainstERP(
	ctx context.Context,
	log *zap.Logger,
	reader StockPositionReader,
	provider interface {
		// GetProductStock devolve o saldo DISPONÍVEL do produto no ERP.
		GetProductStock(ctx context.Context, externalID string) (int, error)
	},
	storeID, externalSource string,
) (*ReconciliationReport, error) {
	positions, err := reader.ListStockPositionsForReconciliation(ctx, storeID, externalSource)
	if err != nil {
		return nil, fmt.Errorf("listing stock positions: %w", err)
	}

	report := &ReconciliationReport{StoreID: storeID}

	for _, p := range positions {
		actual, err := provider.GetProductStock(ctx, p.ExternalID)
		if err != nil {
			// Falha de consulta não é divergência. Contar como tal encheria o
			// relatório de ruído justamente quando o ERP está instável, que é
			// quando ele mais precisa ser legível.
			report.Skipped++
			logger.From(ctx, log).Warn("reconciliation: could not read ERP stock",
				zap.String("product_id", p.ProductID),
				zap.String("external_id", p.ExternalID),
				zap.Error(err))
			continue
		}
		report.Checked++

		expected := p.LocalStock
		if actual == expected {
			continue
		}

		d := StockDivergence{
			ProductID:  p.ProductID,
			Name:       p.Name,
			ExternalID: p.ExternalID,
			Expected:   expected,
			Actual:     actual,
			Delta:      actual - expected,
		}
		report.Divergences = append(report.Divergences, d)

		logger.From(ctx, log).Error("stock divergence between LiveCart and the ERP",
			zap.String("store_id", storeID),
			zap.String("product_id", p.ProductID),
			zap.String("product_name", p.Name),
			zap.String("external_id", p.ExternalID),
			zap.Int("local_stock", p.LocalStock),
			zap.Int("available_in_erp", actual),
			zap.Int("delta", d.Delta),
		)
	}

	if len(report.Divergences) == 0 {
		logger.From(ctx, log).Info("stock reconciliation clean",
			zap.String("store_id", storeID),
			zap.Int("checked", report.Checked),
			zap.Int("skipped", report.Skipped))
	} else {
		logger.From(ctx, log).Error("stock reconciliation found divergences",
			zap.String("store_id", storeID),
			zap.Int("checked", report.Checked),
			zap.Int("skipped", report.Skipped),
			zap.Int("divergences", len(report.Divergences)))
	}

	return report, nil
}
