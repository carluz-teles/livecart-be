package live

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// SetSessionActiveProductRequest define o produto em destaque da transmissão.
// ProductID nil (ou vazio) LIMPA o destaque — é a operação de "clear", que no
// modelo antigo tinha uma query própria.
type SetSessionActiveProductRequest struct {
	ProductID *string `json:"productId"`
}

func (r SetSessionActiveProductRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ProductID, is.UUID),
	)
}

// SetSessionProcessingPausedRequest liga/desliga a pausa da transmissão.
type SetSessionProcessingPausedRequest struct {
	Paused bool `json:"paused"`
}

// Validate existe para satisfazer o contrato do BindAndValidate: um booleano não
// tem forma inválida, mas o handler não pode ter dois caminhos de parse.
func (r SetSessionProcessingPausedRequest) Validate() error { return nil }
