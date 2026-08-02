package integration

// N9/RN-37 — a janela de resposta tardia é 7 dias, o limite do private reply do
// Instagram (e ele vale uma única vez por comentário).
//
// O ponto do teste não é a aritmética: é que webhook e polling CONCORDEM no
// número. O webhook já carregava o timestamp do comentário e o polling não
// carregava nenhum, então o mesmo comentário tinha idade conhecida por um
// caminho e desconhecida pelo outro.

import (
	"testing"
	"time"
)

func TestCommentTooOldToReply(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }

	cases := []struct {
		name string
		ts   int64
		want bool
	}{
		{"comentario de agora", at(0), false},
		{"comentario de 6 dias", at(-6 * 24 * time.Hour), false},
		{"comentario de 7 dias menos uma hora", at(-7*24*time.Hour + time.Hour), false},
		{"comentario de 8 dias", at(-8 * 24 * time.Hour), true},
		{"comentario de 30 dias", at(-30 * 24 * time.Hour), true},

		// Sem carimbo erra para o lado de TENTAR — é o comportamento de hoje, e
		// silenciar por falta de dado seria pior do que uma chamada perdida.
		{"sem carimbo", 0, false},
		{"carimbo negativo", -1, false},

		// Se o carimbo vier em milissegundos (entry.time do webhook), a data cai
		// no futuro e o guard não dispara — de novo, erra para tentar.
		{"carimbo em milissegundos", at(-30*24*time.Hour) * 1000, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentTooOldToReply(tc.ts, now); got != tc.want {
				t.Errorf("commentTooOldToReply = %v, quero %v", got, tc.want)
			}
		})
	}
}

func TestParseGraphTimestamp(t *testing.T) {
	want := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC).Unix()

	cases := map[string]int64{
		// O formato que o Graph devolve de fato em /{media}/comments.
		"2026-08-01T20:00:00+0000": want,
		"2026-08-01T17:00:00-0300": want,
		"2026-08-01T20:00:00Z":     want,
		// Sem carimbo o polling volta ao comportamento anterior: tenta enviar.
		"":         0,
		"ontem":    0,
		"20260801": 0,
	}

	for in, expected := range cases {
		if got := parseGraphTimestamp(in); got != expected {
			t.Errorf("parseGraphTimestamp(%q) = %d, quero %d", in, got, expected)
		}
	}
}
