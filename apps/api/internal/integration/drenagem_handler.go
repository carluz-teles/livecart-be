package integration

// Endpoint da drenagem das reservas manuais — migração única, com prazo.
//
// Fica atrás de um POST porque escreve no ERP, e começa em ensaio (`dryRun`)
// porque a primeira coisa que se quer saber é o tamanho do trabalho: quantos
// carrinhos, quantas unidades, quantas chamadas. Numa loja com live no ar, essa
// resposta decide se a drenagem roda agora ou depois que a live fechar.

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/lib/httpx"
)

// DrainLegacyReservationsRequest é o corpo do pedido de drenagem.
type DrainLegacyReservationsRequest struct {
	// DryRun percorre a lista sem escrever nada. Ponteiro porque `false` é uma
	// resposta VÁLIDA e perigosa: omitir o campo não pode significar "pode
	// escrever no ERP". Ausente vira ensaio.
	DryRun *bool `json:"dryRun"`
	// Limite corta a passada depois de N carrinhos. Zero = todos. Serve para
	// drenar em lotes — o teto da conta é de 30 escritas por minuto.
	//
	// Aceita `limit` e `limite`. O campo nasceu com a tag em inglês e o nome do
	// Go em português, e num ensaio em 29/08 isso custou caro: o cliente mandou
	// `limite`, o JSON ignorou, o valor virou zero — e zero quer dizer TODOS.
	// Num pedido de lote, o erro de digitação escolhia sozinho a opção mais
	// perigosa e irreversível que existe aqui.
	Limite      int `json:"limit"`
	LimiteAlias int `json:"limite"`
}

// Validate é o portão sintático. dryRun fica de fora de propósito: ele é
// OPCIONAL e a ausência tem significado — vira ensaio, que é o lado seguro.
func (r DrainLegacyReservationsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Limite, validation.Min(0), validation.Max(500)),
	)
}

// ToInput traduz o request. Sem value objects: a loja vem do path e já foi
// validada pelo middleware.
func (r DrainLegacyReservationsRequest) ToInput(storeID string) DrainLegacyReservationsInput {
	dryRun := true
	if r.DryRun != nil {
		dryRun = *r.DryRun
	}
	limite := r.Limite
	if limite == 0 {
		limite = r.LimiteAlias
	}
	return DrainLegacyReservationsInput{StoreID: storeID, DryRun: dryRun, Limite: limite}
}

// DrainLegacyReservationsInput é o input do usecase.
type DrainLegacyReservationsInput struct {
	StoreID string
	DryRun  bool
	Limite  int
}

// DrainOutcomeResponse é o que aconteceu com um carrinho.
type DrainOutcomeResponse struct {
	CartID    string `json:"cartId"`
	Units     int    `json:"units"`
	OrderID   string `json:"externalOrderId,omitempty"`
	Reversed  int    `json:"reversed"`
	Remaining int    `json:"remaining"`
	Skipped   string `json:"skipped,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DrainLegacyReservationsResponse é o relatório da passada.
type DrainLegacyReservationsResponse struct {
	DryRun bool `json:"dryRun"`
	// Carts e Units descrevem o trabalho TOTAL pendente, não só o desta passada.
	Carts         int                    `json:"carts"`
	Units         int                    `json:"units"`
	OrdersCreated int                    `json:"ordersCreated"`
	RowsReversed  int                    `json:"rowsReversed"`
	Failed        int                    `json:"failed"`
	TookSeconds   float64                `json:"tookSeconds"`
	Outcomes      []DrainOutcomeResponse `json:"outcomes"`
}

// DrainLegacyReservations troca a guarda do estoque das saídas manuais para os
// pedidos de venda.
//
// @Summary      Drenar reservas manuais para pedidos de venda
// @Description  Migração única: cria o pedido de venda de cada carrinho e só então devolve as saídas manuais. Comece com dryRun.
// @Tags         integrations
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        body body DrainLegacyReservationsRequest true "Opções"
// @Success      200 {object} httpx.Envelope{data=DrainLegacyReservationsResponse}
// @Router       /stores/{storeId}/integrations/erp/drain-legacy-reservations [post]
func (h *Handler) DrainLegacyReservations(c *fiber.Ctx) error {
	var req DrainLegacyReservationsRequest
	if len(c.Body()) > 0 {
		if err := httpx.BindAndValidate(c, &req); err != nil {
			return err
		}
	}
	// Ensaio por omissão: escrever no ERP de uma loja com live no ar é decisão
	// que precisa ser dita em voz alta.
	input := req.ToInput(httpx.GetStoreID(c))

	rel, err := h.service.DrainLegacyReservations(c.UserContext(), input.StoreID, input.DryRun, input.Limite)
	if err != nil {
		return err
	}
	return httpx.OK(c, newDrainResponse(rel))
}

func newDrainResponse(rel *erp.DrainReport) DrainLegacyReservationsResponse {
	out := DrainLegacyReservationsResponse{
		DryRun:        rel.DryRun,
		Carts:         rel.Carts,
		Units:         rel.Units,
		OrdersCreated: rel.OrdersCreated,
		RowsReversed:  rel.RowsReversed,
		Failed:        rel.Failed,
		TookSeconds:   rel.Duration.Seconds(),
		Outcomes:      make([]DrainOutcomeResponse, 0, len(rel.Outcomes)),
	}
	for _, o := range rel.Outcomes {
		out.Outcomes = append(out.Outcomes, DrainOutcomeResponse{
			CartID: o.CartID, Units: o.Units, OrderID: o.OrderID,
			Reversed: o.Reversed, Remaining: o.Remaining,
			Skipped: o.Skipped, Error: o.Err,
		})
	}
	return out
}
