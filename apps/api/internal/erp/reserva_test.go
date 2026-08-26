package erp

// As regras do modo reserva, travadas.
//
// A leitura da flag é o portão de tudo: ligá-la por engano faz uma live parar de
// segurar estoque, e desligá-la por engano faz o pedido nascer com o estoque
// lançado e travar toda mutação seguinte. Por isso ela falha FECHADO em todo
// caso ambíguo.

import (
	"fmt"
	"testing"
)

func TestModoReservaFalhaFechado(t *testing.T) {
	casos := []struct {
		nome     string
		metadata map[string]any
		quer     bool
	}{
		{"metadata nulo", nil, false},
		{"metadata vazio", map[string]any{}, false},
		{"chave ausente", map[string]any{"use_available_stock": true}, false},
		{"booleano true", map[string]any{MetadataOrderReservation: true}, true},
		{"booleano false", map[string]any{MetadataOrderReservation: false}, false},
		{"string true", map[string]any{MetadataOrderReservation: "true"}, true},
		{"string 1", map[string]any{MetadataOrderReservation: "1"}, true},
		{"string false", map[string]any{MetadataOrderReservation: "false"}, false},
		{"string vazia", map[string]any{MetadataOrderReservation: ""}, false},
		{"string lixo", map[string]any{MetadataOrderReservation: "sim"}, false},
		{"numero 1", map[string]any{MetadataOrderReservation: 1}, false},
		{"numero float", map[string]any{MetadataOrderReservation: 1.0}, false},
		{"nulo explicito", map[string]any{MetadataOrderReservation: nil}, false},
		{"objeto", map[string]any{MetadataOrderReservation: map[string]any{"x": 1}}, false},
		{"lista", map[string]any{MetadataOrderReservation: []any{true}}, false},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := metadataFlag(c.metadata, MetadataOrderReservation); got != c.quer {
				t.Errorf("metadataFlag = %v, esperava %v — na dúvida o modo reserva fica DESLIGADO", got, c.quer)
			}
		})
	}
}

// TestFlagsDeMetadataNaoSeConfundem — a integração carrega mais de uma flag no
// mesmo mapa, e ler a errada troca o comportamento inteiro.
func TestFlagsDeMetadataNaoSeConfundem(t *testing.T) {
	metadata := map[string]any{
		"use_available_stock":    true,
		MetadataOrderReservation: false,
		"resync_running_since":   "2026-08-26T00:00:00Z",
		"webhookLastPingAt":      "2026-08-26T00:00:00Z",
	}
	if metadataFlag(metadata, MetadataOrderReservation) {
		t.Error("leu use_available_stock no lugar de order_reservation")
	}
	if !metadataFlag(metadata, "use_available_stock") {
		t.Error("não leu use_available_stock")
	}
}

// TestChaveDoModoReservaEhEstavel — o valor é persistido no banco de cada
// integração; renomear a constante sem migrar desliga o modo silenciosamente
// para quem já migrou.
func TestChaveDoModoReservaEhEstavel(t *testing.T) {
	if MetadataOrderReservation != "order_reservation" {
		t.Fatalf("a chave virou %q — o metadata gravado nas integrações usa %q",
			MetadataOrderReservation, "order_reservation")
	}
}

// TestLeituraDeFlagEhDeterministica sobre todas as formas de entrada.
func TestLeituraDeFlagEhDeterministica(t *testing.T) {
	entradas := []any{true, false, "true", "false", "1", "", 1, 1.0, nil, []any{}, map[string]any{}}
	for i, v := range entradas {
		t.Run(fmt.Sprintf("entrada_%d", i), func(t *testing.T) {
			m := map[string]any{MetadataOrderReservation: v}
			primeiro := metadataFlag(m, MetadataOrderReservation)
			for k := 0; k < 20; k++ {
				if got := metadataFlag(m, MetadataOrderReservation); got != primeiro {
					t.Fatalf("leitura mudou de %v para %v na repetição %d", primeiro, got, k)
				}
			}
		})
	}
}
