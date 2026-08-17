package order

// Portão sintático da edição de itens.
//
// A regra que carrega risco real é o piso da quantidade PAREADO com Required. O
// ozzo pula toda regra (menos Required) quando o valor é o zero-value: com
// `Min(1)` sozinho, um corpo sem o campo — ou com `"quantity": 0` — passaria
// batido. No PATCH isso significaria mandar quantidade 0 ao checkout, que é
// "remover" escrito de um jeito que o endpoint de remover já faz melhor; no POST
// significaria adicionar um item de zero unidade e reservar nada no ERP.

import (
	"errors"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

const produtoValido = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func chaveDoErro(t *testing.T, err error, chave string) {
	t.Helper()
	var verrs validation.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("erro não é validation.Errors (%T) — o ErrorHandler não renderiza 422 com campo", err)
	}
	if _, ok := verrs[chave]; !ok {
		t.Errorf("erro não aponta a chave %q: %v", chave, verrs)
	}
}

func TestAddOrderItemRequestValidate(t *testing.T) {
	casos := []struct {
		nome   string
		req    AddOrderItemRequest
		quebra bool
		chave  string
	}{
		{nome: "produto e quantidade válidos", req: AddOrderItemRequest{ProductID: produtoValido, Quantity: 2}},
		{
			nome:   "sem produto",
			req:    AddOrderItemRequest{Quantity: 1},
			quebra: true, chave: "productId",
		},
		{
			nome:   "produto que não é uuid",
			req:    AddOrderItemRequest{ProductID: "vestido-preto", Quantity: 1},
			quebra: true, chave: "productId",
		},
		{
			// O caso do piso mole: sem Required, este passaria.
			nome:   "quantidade zero é recusada",
			req:    AddOrderItemRequest{ProductID: produtoValido, Quantity: 0},
			quebra: true, chave: "quantity",
		},
		{
			nome:   "quantidade negativa é recusada",
			req:    AddOrderItemRequest{ProductID: produtoValido, Quantity: -1},
			quebra: true, chave: "quantity",
		},
		{
			nome:   "quantidade absurda é recusada antes de chegar ao ERP",
			req:    AddOrderItemRequest{ProductID: produtoValido, Quantity: 1000000},
			quebra: true, chave: "quantity",
		},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			err := tt.req.Validate()
			if !tt.quebra {
				if err != nil {
					t.Fatalf("deveria aceitar, recusou: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("deveria recusar, aceitou")
			}
			chaveDoErro(t, err, tt.chave)
		})
	}
}

func TestSetOrderItemQuantityRequestValidate(t *testing.T) {
	if err := (SetOrderItemQuantityRequest{Quantity: 3}).Validate(); err != nil {
		t.Fatalf("quantidade válida recusada: %v", err)
	}

	// Zero NÃO é "remover": remover é o DELETE. Aceitar zero aqui daria dois
	// caminhos para a mesma coisa, e o do PATCH passaria pelo checkout com uma
	// quantidade que ele também recusa — erro pior, uma ida ao banco no meio.
	for _, q := range []int{0, -2, 1000} {
		err := (SetOrderItemQuantityRequest{Quantity: q}).Validate()
		if err == nil {
			t.Errorf("quantidade %d foi aceita", q)
			continue
		}
		chaveDoErro(t, err, "quantity")
	}
}
