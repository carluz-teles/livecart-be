package live

// =============================================================================
// ENCERRAR UMA SESSÃO — sem encerrar o EVENTO
//
// O evento junta pedidos de dias diferentes; a sessão é a live, o post ou o
// story. Quando a live de segunda acaba, quem tem de parar é a SESSÃO: o evento
// segue no ar até sábado e os carrinhos continuam valendo, porque é exatamente
// isso que faz um comprador de segunda e de terça ter um pedido só.
//
// Até aqui não havia como fazer isso. O único "encerrar" era POST /lives/:id/end,
// que é do evento e faz TRÊS coisas de uma vez: encerra o evento, encerra todas
// as sessões e FINALIZA TODOS OS CARRINHOS PENDENTES. Encerrar a live de segunda
// custava o evento inteiro e levava os carrinhos junto — o oposto da regra.
//
// A metade difícil já existia e estava sem gatilho:
//   - repo.EndSession encerra e emite session.ended na mesma transação;
//   - a ingestão já recusa compra em sessão encerrada (SessionAcceptsPurchase) e
//     responde ao comprador com TypeOutOfWindowSessionEnded, "essa sessão
//     acabou, mas o evento continua" — texto configurável em comunicações.
//
// Este arquivo é só a porta. Nada aqui toca em carrinho nem no evento: essa é a
// regra, decidida pelo dono do produto, e é o que distingue esta rota do
// /end do evento.
// =============================================================================

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// EndSessionInput identifica a sessão a encerrar. Não há corpo: encerrar não
// tem parâmetro, e o evento e a sessão vêm do path — a posse é checada no
// service, nunca no corpo.
type EndSessionInput struct {
	StoreID   string
	EventID   string
	SessionID string
}

// EndSessionOutput é o estado da sessão depois de encerrada.
type EndSessionOutput struct {
	ID      string
	EventID string
	Type    string
	Status  string
}

// EndSession encerra UMA sessão do evento.
//
// Idempotente de propósito: encerrar uma sessão já encerrada devolve o estado
// atual em vez de erro. O botão vive na tela de uma live que acabou de acabar —
// o momento de maior pressa e de clique repetido — e um 400 ali só faria o
// lojista achar que não funcionou.
func (s *Service) EndSession(ctx context.Context, input EndSessionInput) (EndSessionOutput, error) {
	if err := s.resolveSessionOfEvent(ctx, input.SessionID, input.EventID, input.StoreID); err != nil {
		return EndSessionOutput{}, err
	}

	// Sem checagem de nil: resolveSessionOfEvent acima já recusou a sessão que
	// não existe ou que é de outro evento, e GetSessionByID nunca devolve
	// (nil, nil) — ou acha a linha, ou devolve ErrNotFound.
	session, err := s.repo.GetSessionByID(ctx, input.SessionID)
	if err != nil {
		return EndSessionOutput{}, err
	}

	if session.Status == SessionStatusEnded {
		return EndSessionOutput{
			ID:      session.ID,
			EventID: session.EventID,
			Type:    session.Type,
			Status:  session.Status,
		}, nil
	}

	ended, err := s.repo.EndSession(ctx, input.SessionID)
	if err != nil {
		return EndSessionOutput{}, err
	}

	// O evento NÃO é tocado, nem quando esta era a última sessão aberta. Um
	// evento sem sessão ativa é um estado normal: o lojista encerra a live de
	// segunda e pendura a de terça depois, e os carrinhos atravessam o meio.
	logger.From(ctx, s.logger).Info("session ended",
		zap.String("session_id", ended.ID),
		zap.String("event_id", ended.EventID),
		zap.String("type", ended.Type),
	)

	return EndSessionOutput{
		ID:      ended.ID,
		EventID: ended.EventID,
		Type:    ended.Type,
		Status:  ended.Status,
	}, nil
}

// EndSessionResponse é o DTO de saída da rota.
type EndSessionResponse struct {
	ID      string `json:"id"`
	EventID string `json:"eventId"`
	Type    string `json:"type"`
	Status  string `json:"status"`
}

// NewEndSessionResponse mapeia a saída do usecase no DTO de resposta.
func NewEndSessionResponse(out EndSessionOutput) EndSessionResponse {
	return EndSessionResponse{
		ID:      out.ID,
		EventID: out.EventID,
		Type:    out.Type,
		Status:  out.Status,
	}
}

// EndSession godoc
// @Summary      Encerrar uma sessão
// @Description  Encerra a live/post/story SEM encerrar o evento e sem finalizar carrinhos.
// @Tags         lives
// @Produce      json
// @Param        storeId   path string true "Store ID"
// @Param        id        path string true "Event ID"
// @Param        sessionId path string true "Session ID"
// @Success      200 {object} httpx.Envelope{data=EndSessionResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions/{sessionId}/end [post]
// @Security     BearerAuth
func (h *Handler) EndSession(c *fiber.Ctx) error {
	out, err := h.service.EndSession(c.UserContext(), EndSessionInput{
		StoreID:   c.Locals("store_id").(string),
		EventID:   c.Params("id"),
		SessionID: c.Params("sessionId"),
	})
	if err != nil {
		return err
	}
	return httpx.OK(c, NewEndSessionResponse(out))
}
