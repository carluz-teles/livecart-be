package live

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// =============================================================================
// SESSION PRODUCTS (Whitelist da SESSÃO — D15/N2)
//
// Regra única em todo o sistema: lista vazia = TODOS os produtos da loja
// liberados naquela sessão. Sessão nova nasce vazia e, portanto, vende tudo.
//
// DTOs novos seguem a skill api-domain-convention (ozzo + Validate/ToInput) —
// a isenção do convention_test vale só para os DTOs antigos do domínio live.
// =============================================================================

// SessionProductOutput é a whitelist na sua unidade nova. Mesma forma do
// EventProductOutput porque é o mesmo dado, só que ancorado na sessão — alias,
// não cópia, para não criar uma segunda fonte da verdade do formato.
type SessionProductOutput = EventProductOutput

// SessionProductRequest é o corpo de POST/PUT da whitelist da sessão.
// O produto é identificado no CORPO no POST e no PATH no PUT.
type SessionProductRequest struct {
	ProductID    string `json:"productId"`
	SpecialPrice *int64 `json:"specialPrice"`
	MaxQuantity  *int32 `json:"maxQuantity"`
	DisplayOrder int32  `json:"displayOrder"`
	Featured     bool   `json:"featured"`
}

func (r SessionProductRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ProductID, validation.Required, is.UUID),
		validation.Field(&r.SpecialPrice, validation.Min(int64(0))),
		// NilOrNotEmpty por causa do gotcha do ozzo: num ponteiro, o 0 explícito
		// é zero-value e faria o Min ser PULADO — teto zero passaria batido.
		validation.Field(&r.MaxQuantity, validation.NilOrNotEmpty, validation.Min(int32(1))),
		validation.Field(&r.DisplayOrder, validation.Min(0)),
	)
}

// ToInput traduz o Request no input do usecase. productID vindo do path vence o
// do corpo (é o PUT); vazio significa "usa o do corpo" (é o POST).
func (r SessionProductRequest) ToInput(storeID, eventID, sessionID, productID string) SessionProductInput {
	if productID == "" {
		productID = r.ProductID
	}
	return SessionProductInput{
		StoreID:      storeID,
		EventID:      eventID,
		SessionID:    sessionID,
		ProductID:    productID,
		SpecialPrice: r.SpecialPrice,
		MaxQuantity:  r.MaxQuantity,
		DisplayOrder: r.DisplayOrder,
		Featured:     r.Featured,
	}
}

// UpdateSessionProductRequest é o PUT: o produto vem no path, não no corpo.
type UpdateSessionProductRequest struct {
	SpecialPrice *int64 `json:"specialPrice"`
	MaxQuantity  *int32 `json:"maxQuantity"`
	DisplayOrder int32  `json:"displayOrder"`
	Featured     bool   `json:"featured"`
}

func (r UpdateSessionProductRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.SpecialPrice, validation.Min(int64(0))),
		// NilOrNotEmpty por causa do gotcha do ozzo: num ponteiro, o 0 explícito
		// é zero-value e faria o Min ser PULADO — teto zero passaria batido.
		validation.Field(&r.MaxQuantity, validation.NilOrNotEmpty, validation.Min(int32(1))),
		validation.Field(&r.DisplayOrder, validation.Min(0)),
	)
}

func (r UpdateSessionProductRequest) ToInput(storeID, eventID, sessionID, productID string) SessionProductInput {
	return SessionProductInput{
		StoreID:      storeID,
		EventID:      eventID,
		SessionID:    sessionID,
		ProductID:    productID,
		SpecialPrice: r.SpecialPrice,
		MaxQuantity:  r.MaxQuantity,
		DisplayOrder: r.DisplayOrder,
		Featured:     r.Featured,
	}
}

// SessionProductInput é o input do usecase, para POST e PUT (é um upsert: a
// chave natural é (sessão, produto), não o id da linha — foi chavear pelo id da
// linha que quebrou o par FE/BE na whitelist do evento).
type SessionProductInput struct {
	StoreID      string
	EventID      string
	SessionID    string
	ProductID    string
	SpecialPrice *int64
	MaxQuantity  *int32
	DisplayOrder int32
	Featured     bool
}

// ListSessionProductsResponse envelopa a whitelist da sessão.
type ListSessionProductsResponse struct {
	Data []EventProductResponse `json:"data"`
}
