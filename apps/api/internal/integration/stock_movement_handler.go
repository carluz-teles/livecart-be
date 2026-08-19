package integration

// Endpoints do painel de pendências do razão de movimentos.
//
// A decisão que eles carregam é humana por necessidade: a API v3 do Tiny não
// tem consulta de lançamentos, então o desempate de um movimento ambíguo é o
// extrato do produto na tela, filtrado pela chave de idempotência. Antes destes
// endpoints, a resposta virava UPDATE manual em produção.

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/lib/httpx"
)

// PendingStockMovementResponse é a linha do painel.
type PendingStockMovementResponse struct {
	ID             string `json:"id"`
	Direction      string `json:"direction"`
	Status         string `json:"status"`
	Quantity       int    `json:"quantity"`
	IdempotencyKey string `json:"idempotencyKey"`
	ProductName    string `json:"productName"`
	ProductKeyword string `json:"productKeyword"`
	ExternalID     string `json:"externalProductId"`
	CartID         string `json:"cartId"`
	CartHandle     string `json:"cartHandle"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"lastError"`
	CreatedAt      string `json:"createdAt"`
}

func newPendingStockMovementResponse(m erp.PendingStockMovement) PendingStockMovementResponse {
	return PendingStockMovementResponse{
		ID:             m.ID,
		Direction:      m.Direction,
		Status:         m.Status,
		Quantity:       m.Quantity,
		IdempotencyKey: m.IdempotencyKey,
		ProductName:    m.ProductName,
		ProductKeyword: m.ProductKeyword,
		ExternalID:     m.ExternalProductID,
		CartID:         m.CartID,
		CartHandle:     m.CartHandle,
		Attempts:       m.Attempts,
		LastError:      m.LastError,
		CreatedAt:      m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// ListPendingStockMovements lista os movimentos parados da loja.
func (h *Handler) ListPendingStockMovements(c *fiber.Ctx) error {
	rows, err := h.service.ListPendingStockMovements(c.UserContext(), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	out := make([]PendingStockMovementResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, newPendingStockMovementResponse(r))
	}
	return httpx.OK(c, out)
}

// ResolveStockMovementRequest carrega a decisão humana pós-extrato.
type ResolveStockMovementRequest struct {
	// Landed responde "o lançamento está no extrato?". Ponteiro porque false é
	// resposta VÁLIDA (não entrou) e precisa ser distinguível de omitido.
	Landed *bool `json:"landed"`
}

// Validate é o portão sintático (ozzo puro).
func (r ResolveStockMovementRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Landed, validation.NotNil),
	)
}

// ResolveStockMovementInput é o input do usecase.
type ResolveStockMovementInput struct {
	StoreID    string
	MovementID string
	Landed     bool
}

// ToInput traduz o request; sem value objects aqui — os ids são validados pelo
// parse de UUID no repositório (404 semântico).
func (r ResolveStockMovementRequest) ToInput(storeID, movementID string) (ResolveStockMovementInput, error) {
	return ResolveStockMovementInput{StoreID: storeID, MovementID: movementID, Landed: *r.Landed}, nil
}

// ResolveStockMovementResponse é o estado final do movimento.
type ResolveStockMovementResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ResolveStockMovement aplica a decisão sobre um movimento parado.
func (h *Handler) ResolveStockMovement(c *fiber.Ctx) error {
	var req ResolveStockMovementRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), c.Params("movementId"))
	if err != nil {
		return err
	}
	row, err := h.service.ResolveStockMovementManually(c.UserContext(), input.StoreID, input.MovementID, input.Landed)
	if err != nil {
		return err
	}
	return httpx.OK(c, ResolveStockMovementResponse{ID: row.ID, Status: row.Status})
}

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
