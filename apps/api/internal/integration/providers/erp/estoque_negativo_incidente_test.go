package erp

// Blindagem contra o estoque negativo de 20/08/2026 (1268/1130 cantodaart).
//
// O Alisson pediu "todos os testes possíveis, isso jamais pode ocorrer de
// novo". A cadeia do bug tinha um único elo quebrado: o parser do saldo
// disponível descartava um disponível NEGATIVO e o chamador caía no saldo
// FÍSICO bruto (positivo), que virava products.stock e alimentava a promoção
// da fila sobre unidade inexistente. Estes testes usam os CORPOS REAIS que o
// Tiny devolveu no incidente (copiados do log de produção) e provam, byte a
// byte, que o disponível negativo satura em 0 (esgotado).

import (
	"encoding/json"
	"testing"
)

// corpo real do Tiny para o 1268 (idProduto 845330820), do log de 20/08 17:30.
const corpoTiny1268 = `{"id":845330820,"nome":"Pote de Vidro com Tampa Hermética Natalino Boneco de Neve - 300ml ","codigo":"YB4788BN","unidade":"pc","saldo":1,"reservado":2,"disponivel":-1,"localizacao":"","depositos":[{"id":830288141,"nome":"galpão (estoque)","desconsiderar":false,"saldo":0,"reservado":2,"disponivel":-2,"empresa":"cantodaart"},{"id":627680559,"nome":"loja","desconsiderar":false,"saldo":1,"reservado":2,"disponivel":-1,"empresa":"cantodaart"}]}`

// mais tarde no mesmo minuto, com o galpão e a loja zerados: disponível -2.
const corpoTiny1268Pior = `{"id":845330820,"nome":"Pote ...","codigo":"YB4788BN","unidade":"pc","saldo":0,"reservado":2,"disponivel":-2,"localizacao":"","depositos":[]}`

func extrairDoCorpo(t *testing.T, corpo string) (int, string, bool) {
	t.Helper()
	var cru map[string]any
	if err := json.Unmarshal([]byte(corpo), &cru); err != nil {
		t.Fatalf("unmarshal do corpo do Tiny: %v", err)
	}
	return ExtrairSaldoDisponivel(cru)
}

func TestIncidente1268CorpoRealSaturaEmZero(t *testing.T) {
	// O corpo que promoveu a @livia sobre unidade fantasma. Com o fix, ele lê
	// 0 (esgotado) — o backstop da fila, cujo gate é um decremento atômico de
	// products.stock, não teria o que promover.
	saldo, campo, ok := extrairDoCorpo(t, corpoTiny1268)
	if !ok {
		t.Fatal("corpo real do 1268 recusado — cairia no saldo físico e promoveria a fila")
	}
	if campo != "disponivel" {
		t.Errorf("campo=%q; esperava 'disponivel' (nunca cair no saldo bruto)", campo)
	}
	if saldo != 0 {
		t.Errorf("saldo=%d; esperava 0 (disponível real era -1)", saldo)
	}

	saldo2, _, ok2 := extrairDoCorpo(t, corpoTiny1268Pior)
	if !ok2 || saldo2 != 0 {
		t.Errorf("corpo -2: saldo=%d ok=%v; esperava 0/true", saldo2, ok2)
	}
}

// Tabela exaustiva: todo o espectro de respostas do endpoint de estoque do
// Tiny. O que promove é SÓ disponível positivo real; tudo que representa
// esgotado (0 ou negativo) tem de virar 0, e o que não é número não afirma
// nada (o chamador preserva o físico).
func TestEspectroDeSaldoDisponivel(t *testing.T) {
	casos := []struct {
		nome     string
		payload  map[string]any
		wantOK   bool
		wantSald int
	}{
		{"disponível positivo promove", map[string]any{"disponivel": float64(3)}, true, 3},
		{"disponível zero é esgotado", map[string]any{"disponivel": float64(0)}, true, 0},
		{"disponível -1 satura em 0", map[string]any{"disponivel": float64(-1)}, true, 0},
		{"disponível -50 satura em 0", map[string]any{"disponivel": float64(-50)}, true, 0},
		{"overselling saldo>0 disp<0", map[string]any{"saldo": float64(1), "reservado": float64(2), "disponivel": float64(-1)}, true, 0},
		{"disponível textual não afirma", map[string]any{"disponivel": "3"}, false, 0},
		{"disponível ausente não afirma", map[string]any{"saldo": float64(4), "reservado": float64(1)}, false, 0},
		{"resposta vazia não afirma", map[string]any{}, false, 0},
		{"disponível fracionário trunca", map[string]any{"disponivel": float64(2.9)}, true, 2},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			saldo, _, ok := ExtrairSaldoDisponivel(c.payload)
			if ok != c.wantOK {
				t.Fatalf("ok=%v; esperava %v", ok, c.wantOK)
			}
			if ok && saldo != c.wantSald {
				t.Errorf("saldo=%d; esperava %d", saldo, c.wantSald)
			}
		})
	}
}
