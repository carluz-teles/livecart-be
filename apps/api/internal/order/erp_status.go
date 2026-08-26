package order

// O trajeto do pedido dentro do ERP, visível no LiveCart.
//
// O pedido de venda nasce no primeiro comentário da live e continua se movendo
// muito depois dela: aprovado no pagamento, faturado, separado, despachado,
// entregue. Até aqui esse trajeto existia só no ERP — quem quisesse responder
// "esse pedido já saiu?" abria o outro sistema.

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// ERPOrderStatusEntry é uma passagem do pedido por um estágio do ERP.
type ERPOrderStatusEntry struct {
	Status         string
	PreviousStatus string
	OrderNumber    string
	Source         string
	ObservedAt     time.Time
}

// ERPOrderStatusReader é satisfeito por integration.Service. Dependência
// invertida pelo mesmo motivo de ERPFinalisationRetrier: o pacote order não
// importa integration.
type ERPOrderStatusReader interface {
	ListERPOrderStatusHistory(ctx context.Context, cartID string) ([]ERPOrderStatusEntry, error)
}

// SetERPOrderStatusReader liga a leitura do rastreamento (injeção no boot).
func (s *Service) SetERPOrderStatusReader(r ERPOrderStatusReader) { s.erpStatusReader = r }

// ERPOrderStatusHistory devolve o trajeto do pedido, do mais recente para o mais
// antigo. Valida o escopo da loja antes de ler — mesma fronteira do retry.
func (s *Service) ERPOrderStatusHistory(ctx context.Context, orderID, storeID string) ([]ERPOrderStatusEntry, error) {
	if s.erpStatusReader == nil {
		return nil, httpx.DomainError(422, httpx.CodeErpRetryUnavailable, "rastreamento de ERP indisponível")
	}
	if _, err := s.GetByID(ctx, orderID, storeID); err != nil {
		return nil, err
	}
	return s.erpStatusReader.ListERPOrderStatusHistory(ctx, orderID)
}

// ERPOrderStatusEntryResponse é uma linha do trajeto na resposta HTTP.
type ERPOrderStatusEntryResponse struct {
	Status         string `json:"status"`
	PreviousStatus string `json:"previousStatus,omitempty"`
	OrderNumber    string `json:"orderNumber,omitempty"`
	// Source diz quem observou: "webhook" quando o ERP avisou, "sweep" quando
	// fomos nós que perguntamos. Uma sequência de "sweep" é sintoma de webhook
	// que parou de chegar, e o lojista consegue ver isso sem abrir log.
	Source     string `json:"source"`
	ObservedAt string `json:"observedAt"`
}

// ERPOrderStatusHistory godoc
// @Summary      Trajeto do pedido no ERP
// @Description  Situações pelas quais o pedido de venda passou no ERP, da mais recente para a mais antiga.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order UUID"
// @Success      200 {object} httpx.Envelope{data=[]ERPOrderStatusEntryResponse}
// @Router       /stores/{storeId}/orders/{id}/erp-status [get]
func (h *Handler) ERPOrderStatusHistory(c *fiber.Ctx) error {
	entries, err := h.service.ERPOrderStatusHistory(c.UserContext(), c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	out := make([]ERPOrderStatusEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, ERPOrderStatusEntryResponse{
			Status:         e.Status,
			PreviousStatus: e.PreviousStatus,
			OrderNumber:    e.OrderNumber,
			Source:         e.Source,
			ObservedAt:     e.ObservedAt.UTC().Format(time.RFC3339),
		})
	}
	return httpx.OK(c, out)
}
