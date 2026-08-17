package customer

// Busca de arrobas vistos nas lives da loja — o insumo da tela de perfis
// bloqueados.
//
// Por que não reusar a busca de `customers`: aquela tabela só ganha linha depois
// de um pedido. O perfil que o lojista mais precisa bloquear é a conta secundária
// da PRÓPRIA loja, usada para instruir a audiência ("manda 1042 2"), e ela pode
// nunca ter comprado nada. Quem tem a plateia inteira é live_comments — e o
// mesmo funil recebe comentário de live, comentário de post e resposta de story
// (que chega por DM), então "qualquer canal" já está coberto pela fonte.

import (
	"context"
	"errors"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/customer/domain"
	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// REQUEST / INPUT / RESPONSE
// =============================================================================

// searchHandlesMinTerm é o piso do termo de busca.
//
// Dois caracteres é o menor trecho que ainda discrimina. Com um caractere a
// consulta devolveria quase toda a plateia — que é justamente o que a tela NÃO
// deve fazer: o lojista chega aqui sabendo quem procura, e uma lista de
// centenas de arrobas transforma "bloquear a conta certa" em caça ao erro.
const searchHandlesMinTerm = 2

// searchHandlesMaxResults limita o retorno. Passar disto significa que o termo
// está curto demais para decidir, e a tela pede um trecho maior em vez de
// oferecer uma parede de perfis.
const searchHandlesMaxResults = 25

// SearchHandlesRequest é a query string da busca.
//
// A tag `json` acompanha a `query` porque é dela que o ozzo tira a chave do erro
// de validação. Sem ela o 422 sairia com "Term" — nome do campo Go — e o front
// não teria como ligar a mensagem ao input que o lojista digitou.
type SearchHandlesRequest struct {
	Term  string `query:"search" json:"search"`
	Exact bool   `query:"exact" json:"exact"`
}

// Validate é o portão sintático (ozzo). O termo é obrigatório: sem ele a busca
// não roda, em vez de devolver a lista inteira.
//
// O piso mede o termo JÁ NORMALIZADO, não o que veio na URL. "@a" tem duas
// runas e passaria um RuneLength cru, mas normaliza para "a"; "@" sozinho
// normaliza para vazio, e substring vazia casa a plateia inteira. Medir aqui
// mantém a recusa como validation.Errors — o 422 sai com `fields.search` e o
// front destaca o input em vez de mostrar um toast solto.
func (r SearchHandlesRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Term,
			validation.Required,
			validation.RuneLength(searchHandlesMinTerm, 30),
			validation.By(termoUtilAposNormalizar),
		),
	)
}

// termoUtilAposNormalizar recusa o termo que perde substância ao virar arroba.
func termoUtilAposNormalizar(value any) error {
	termo, _ := value.(string)
	if len([]rune(normalizeHandle(termo))) < searchHandlesMinTerm {
		return errors.New("informe pelo menos 2 caracteres do arroba, sem contar o @")
	}
	return nil
}

// ToInput normaliza o arroba digitado. O lojista cola "@Fulano" com o @ e às
// vezes com maiúscula; o handle gravado é minúsculo e sem @. Normalizar aqui —
// e não na query — mantém uma única definição de "o que é um arroba" no domínio,
// a mesma de normalizeHandle usada por bloquear/desbloquear.
func (r SearchHandlesRequest) ToInput(storeID string) (SearchHandlesInput, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return SearchHandlesInput{}, httpx.ErrUnprocessable("invalid store id")
	}

	return SearchHandlesInput{
		StoreID: storeUUID,
		Term:    normalizeHandle(r.Term),
		Exact:   r.Exact,
		Limit:   searchHandlesMaxResults,
	}, nil
}

type SearchHandlesInput struct {
	StoreID uuid.UUID
	Term    string
	Exact   bool
	Limit   int32
}

// SearchedHandleResponse é um arroba encontrado na plateia da loja.
type SearchedHandleResponse struct {
	Handle string `json:"handle"`
	// MessageCount são todas as mensagens dele nas lives da loja.
	MessageCount int `json:"messageCount"`
	// OrderMessageCount são as que viraram item no carrinho. É o número que
	// identifica a conta de instrução: fala muito E gera pedido.
	OrderMessageCount int        `json:"orderMessageCount"`
	LastSeenAt        *time.Time `json:"lastSeenAt,omitempty"`
	Blocked           bool       `json:"blocked"`
}

// NewSearchedHandlesResponse mapeia as entidades para a resposta da API.
func NewSearchedHandlesResponse(handles []*domain.SeenHandle) []SearchedHandleResponse {
	out := make([]SearchedHandleResponse, len(handles))
	for i, h := range handles {
		out[i] = SearchedHandleResponse{
			Handle:            h.Handle(),
			MessageCount:      h.MessageCount(),
			OrderMessageCount: h.OrderMessageCount(),
			LastSeenAt:        h.LastSeenAt(),
			Blocked:           h.Blocked(),
		}
	}
	return out
}

// =============================================================================
// REPOSITORY
// =============================================================================

// SearchSeenHandles devolve os arrobas da plateia que casam com o termo.
func (r *Repository) SearchSeenHandles(ctx context.Context, input SearchHandlesInput) ([]*domain.SeenHandle, error) {
	rows, err := r.queries.SearchStoreHandles(ctx, sqlc.SearchStoreHandlesParams{
		StoreID:    uuidToPgtype(input.StoreID),
		Term:       input.Term,
		ExactMatch: input.Exact,
		RowLimit:   input.Limit,
	})
	if err != nil {
		return nil, err
	}

	out := make([]*domain.SeenHandle, 0, len(rows))
	for _, row := range rows {
		var lastSeen *time.Time
		if row.LastSeenAt.Valid {
			t := row.LastSeenAt.Time
			lastSeen = &t
		}
		out = append(out, domain.ReconstructSeenHandle(
			row.Handle,
			int(row.MessageCount),
			int(row.CartMessageCount),
			lastSeen,
			row.Blocked,
		))
	}
	return out, nil
}

// =============================================================================
// SERVICE
// =============================================================================

// SearchSeenHandles busca arrobas da plateia da loja.
func (s *Service) SearchSeenHandles(ctx context.Context, input SearchHandlesInput) ([]*domain.SeenHandle, error) {
	return s.repo.SearchSeenHandles(ctx, input)
}

// =============================================================================
// HANDLER
// =============================================================================

// SearchHandles godoc
// @Summary      Search handles seen in the store's lives
// @Description  Searches Instagram handles that have sent any message in this store's lives, so the merchant can block them. Requires a search term — there is no full listing.
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string true "Handle or part of it (min 2 chars)"
// @Param        exact query bool false "Match the whole handle instead of a substring"
// @Success      200 {object} httpx.Envelope{data=[]SearchedHandleResponse}
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/customers/handles [get]
// @Security     BearerAuth
func (h *Handler) SearchHandles(c *fiber.Ctx) error {
	var req SearchHandlesRequest
	if err := c.QueryParser(&req); err != nil {
		return httpx.ErrBadRequest("invalid query parameters")
	}
	// O termo chega da URL: espaço em volta é do usuário colando, não intenção.
	req.Term = strings.TrimSpace(req.Term)
	if err := req.Validate(); err != nil {
		return err
	}

	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}

	handles, err := h.service.SearchSeenHandles(c.UserContext(), input)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewSearchedHandlesResponse(handles))
}
