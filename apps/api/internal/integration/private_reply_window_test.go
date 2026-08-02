package integration

// N9/RN-37 — a janela de resposta tardia é 7 dias, o limite do private reply do
// Instagram (e ele vale uma única vez por comentário).
//
// O ponto do teste não é a aritmética: é que webhook e polling CONCORDEM no
// número. O webhook já carregava o timestamp do comentário e o polling não
// carregava nenhum, então o mesmo comentário tinha idade conhecida por um
// caminho e desconhecida pelo outro.

import (
	"encoding/json"
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

		// E41: carimbo em milissegundos (entry.time do webhook e timestamp da
		// Messaging API) é NORMALIZADO. Antes, time.Unix(1.75e12, 0) caía no
		// ano ~57000, a idade dava negativa e o guard nunca disparava: a janela
		// de 7 dias só valia no polling.
		{"carimbo em milissegundos, comentario velho", at(-30*24*time.Hour) * 1000, true},
		{"carimbo em milissegundos, comentario de agora", at(0) * 1000, false},
		{"carimbo em milissegundos, 6 dias", at(-6*24*time.Hour) * 1000, false},
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

// E41 — de onde sai o carimbo do comentário no caminho do WEBHOOK.
//
// O handler mandava entry.Time cru, que é (a) o instante da ENTREGA, não do
// comentário, e (b) milissegundos nos produtos de Instagram. As duas coisas
// juntas faziam a janela de 7 dias existir só no polling.
func TestCommentedAtPrefereOCarimboDoComentario(t *testing.T) {
	const entryMs = int64(1_754_049_600_000) // 2026-08-01T12:00:00Z em ms
	const entrySec = entryMs / 1000
	const commentSec = entrySec - 3*24*60*60

	cases := []struct {
		name  string
		value InstagramLiveCommentValue
		entry int64
		want  int64
	}{
		{
			name:  "sem carimbo proprio cai no entry.time, normalizado de ms",
			value: InstagramLiveCommentValue{},
			entry: entryMs,
			want:  entrySec,
		},
		{
			name:  "entry.time ja em segundos passa intacto",
			value: InstagramLiveCommentValue{},
			entry: entrySec,
			want:  entrySec,
		},
		{
			// A entrega pode ser dias mais nova que o comentário (redelivery,
			// backlog de fila). Quem decide a janela é a idade do COMENTÁRIO.
			name:  "carimbo do comentario vence o da entrega",
			value: InstagramLiveCommentValue{Timestamp: commentTime(commentSec)},
			entry: entryMs,
			want:  commentSec,
		},
		{
			name:  "created_time em ms tambem serve",
			value: InstagramLiveCommentValue{CreatedTime: commentTime(commentSec * 1000)},
			entry: entryMs,
			want:  commentSec,
		},
		{
			name:  "sem nada devolve 0 e o chamador tenta enviar",
			value: InstagramLiveCommentValue{},
			entry: 0,
			want:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.value.CommentedAt(tc.entry); got != tc.want {
				t.Errorf("CommentedAt = %d, quero %d", got, tc.want)
			}
		})
	}
}

// A Meta manda `created_time` ora como número, ora como string ISO8601. Um
// unmarshal estrito derrubaria o payload INTEIRO por causa do carimbo — ou
// seja, perderia o comentário, não só a idade dele.
func TestCommentTimeAceitaNumeroEString(t *testing.T) {
	want := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC).Unix()

	cases := map[string]int64{
		`{"created_time":1785614400}`:                 want,
		`{"created_time":1785614400000}`:              want * 1000, // normalizado só no CommentedAt
		`{"created_time":"2026-08-01T20:00:00+0000"}`: want,
		`{"created_time":null}`:                       0,
		`{"created_time":"ontem"}`:                    0,
		`{}`:                                          0,
	}

	for raw, expected := range cases {
		var v InstagramLiveCommentValue
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("unmarshal %s: %v — o comentario inteiro seria perdido", raw, err)
		}
		if int64(v.CreatedTime) != expected {
			t.Errorf("%s → created_time = %d, quero %d", raw, int64(v.CreatedTime), expected)
		}
	}
}
