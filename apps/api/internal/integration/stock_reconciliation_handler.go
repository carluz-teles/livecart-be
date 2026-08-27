package integration

// Reconciliação de estoque: comparar o contador local com o saldo DISPONÍVEL do
// ERP, produto a produto.
//
// A conta virou uma igualdade quando as reservas manuais saíram: o carrinho
// aberto desconta do contador daqui e, porque virou pedido de venda, desconta do
// disponível de lá. Divergência agora significa alguma coisa de verdade — venda
// no balcão, ajuste manual, ou defeito nosso.

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// StockReconciliationResponse é o relatório da comparação local × ERP.
type StockReconciliationResponse struct {
	Checked     int                       `json:"checked"`
	Skipped     int                       `json:"skipped"`
	Divergences []StockDivergenceResponse `json:"divergences"`
}

type StockDivergenceResponse struct {
	ProductID  string `json:"productId"`
	Name       string `json:"name"`
	ExternalID string `json:"externalProductId"`
	Expected   int    `json:"expected"`
	Actual     int    `json:"actual"`
	Delta      int    `json:"delta"`
}

// RunStockReconciliation compara a contabilidade local com o saldo do ERP,
// produto a produto. Só leitura; o relatório é a resposta.
func (h *Handler) RunStockReconciliation(c *fiber.Ctx) error {
	report, err := h.service.RunStockReconciliation(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	out := StockReconciliationResponse{
		Checked:     report.Checked,
		Skipped:     report.Skipped,
		Divergences: make([]StockDivergenceResponse, 0, len(report.Divergences)),
	}
	for _, d := range report.Divergences {
		out.Divergences = append(out.Divergences, StockDivergenceResponse{
			ProductID: d.ProductID, Name: d.Name, ExternalID: d.ExternalID,
			Expected: d.Expected, Actual: d.Actual, Delta: d.Delta,
		})
	}
	return httpx.OK(c, out)
}
