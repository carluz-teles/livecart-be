package integration

import (
	"testing"
)

func TestParsePurchaseIntent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNil  bool
		wantQty  int
	}{
		// Positive cases - should detect purchase intent
		{name: "quero simples", input: "quero", wantNil: false, wantQty: 1},
		{name: "quero com quantidade", input: "quero 2", wantNil: false, wantQty: 2},
		{name: "quero com quantidade e texto", input: "quero 3 unidades", wantNil: false, wantQty: 3},
		{name: "eu quero", input: "eu quero", wantNil: false, wantQty: 1},
		{name: "eu quero com quantidade", input: "eu quero 5", wantNil: false, wantQty: 5},
		{name: "reserva", input: "reserva 2 pra mim", wantNil: false, wantQty: 2},
		{name: "manda", input: "manda 1", wantNil: false, wantQty: 1},
		{name: "separa", input: "separa 4 pra mim", wantNil: false, wantQty: 4},
		{name: "X unidades", input: "5 unidades por favor", wantNil: false, wantQty: 5},
		{name: "X unidade singular", input: "1 unidade", wantNil: false, wantQty: 1},
		{name: "pega", input: "pega 2 pra mim", wantNil: false, wantQty: 2},
		{name: "me manda", input: "me manda 3", wantNil: false, wantQty: 3},
		{name: "coloca", input: "coloca 2", wantNil: false, wantQty: 2},
		{name: "case insensitive", input: "QUERO 2", wantNil: false, wantQty: 2},
		{name: "mixed case", input: "Quero 3 Unidades", wantNil: false, wantQty: 3},

		// Negative cases - should NOT detect purchase intent
		{name: "nao quero", input: "não quero", wantNil: true, wantQty: 0},
		{name: "cancela", input: "cancela meu pedido", wantNil: true, wantQty: 0},
		{name: "desisto", input: "desisto", wantNil: true, wantQty: 0},
		{name: "quanto custa", input: "quanto custa?", wantNil: true, wantQty: 0},
		{name: "qual o preco", input: "qual o preço?", wantNil: true, wantQty: 0},
		{name: "tem desconto", input: "tem desconto?", wantNil: true, wantQty: 0},
		{name: "ainda tem", input: "ainda tem?", wantNil: true, wantQty: 0},
		{name: "random text", input: "olá, o produto é muito bom", wantNil: true, wantQty: 0},
		{name: "empty", input: "", wantNil: true, wantQty: 0},
		{name: "only spaces", input: "   ", wantNil: true, wantQty: 0},

		// Edge cases
		{name: "quantity over 100 capped", input: "quero 99", wantNil: false, wantQty: 99},
		{name: "zero quantity", input: "quero 0", wantNil: false, wantQty: 1}, // 0 is invalid, defaults to 1

		// Keyword as number - should NOT interpret keyword as quantity
		{name: "quero keyword 1001", input: "quero 1001", wantNil: false, wantQty: 1},  // keyword detected, qty defaults to 1
		{name: "quero keyword 1000", input: "quero 1000", wantNil: false, wantQty: 1},  // keyword detected, qty defaults to 1
		{name: "2x keyword", input: "2x 1001", wantNil: false, wantQty: 2},             // explicit qty before keyword
		{name: "keyword 3x", input: "1001 3x", wantNil: false, wantQty: 3},             // explicit qty after keyword
		{name: "quero 2 keyword", input: "quero 2 1001", wantNil: false, wantQty: 2},   // explicit qty with keyword
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParsePurchaseIntent(tt.input)

			if tt.wantNil {
				if result != nil {
					t.Errorf("ParsePurchaseIntent(%q) = %+v, want nil", tt.input, result)
				}
				return
			}

			if result == nil {
				t.Errorf("ParsePurchaseIntent(%q) = nil, want quantity %d", tt.input, tt.wantQty)
				return
			}

			if result.Quantity != tt.wantQty {
				t.Errorf("ParsePurchaseIntent(%q).Quantity = %d, want %d", tt.input, result.Quantity, tt.wantQty)
			}
		})
	}
}

func TestIsCancellation(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "cancela", input: "cancela", want: true},
		{name: "desisto", input: "desisto", want: true},
		{name: "nao quero mais", input: "não quero mais", want: true},
		{name: "tira o meu", input: "tira o meu pedido", want: true},
		{name: "remove", input: "remove por favor", want: true},
		{name: "quero", input: "quero 2", want: false},
		{name: "random", input: "oi tudo bem?", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsCancellation(tt.input)
			if result != tt.want {
				t.Errorf("IsCancellation(%q) = %v, want %v", tt.input, result, tt.want)
			}
		})
	}
}

func TestExtractPossibleKeywords(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "simple keyword", input: "quero 2 A9B1", want: []string{"A9B1"}},
		{name: "lowercase", input: "quero a9b1", want: []string{"A9B1"}},
		{name: "multiple keywords", input: "quero X1Y2 e Z3W4", want: []string{"X1Y2", "Z3W4"}},
		{name: "no keyword", input: "quero 2 unidades", want: nil},
		{name: "only letters", input: "quero AZUL", want: nil},
		{name: "only numbers", input: "quero 1234", want: nil},
		{name: "mixed valid", input: "reserva 3 do 1A2B pra mim", want: []string{"1A2B"}},
		{name: "keyword at start", input: "A1B2 quero", want: []string{"A1B2"}},
		{name: "duplicate keyword", input: "quero A9B1 manda A9B1", want: []string{"A9B1"}},
		{name: "empty", input: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractPossibleKeywords(tt.input)
			if len(result) != len(tt.want) {
				t.Errorf("ExtractPossibleKeywords(%q) = %v, want %v", tt.input, result, tt.want)
				return
			}
			for i, v := range result {
				if v != tt.want[i] {
					t.Errorf("ExtractPossibleKeywords(%q)[%d] = %v, want %v", tt.input, i, v, tt.want[i])
				}
			}
		})
	}
}
