package live

// D18/D19/D20 — a decisão "esta campanha aceita compra agora?".
//
// Estes casos vieram de integration/event_window_test.go, que testava uma
// SEGUNDA implementação da mesma decisão. As duas divergiam: a da ingestão não
// tratava status='scheduled' sem data futura (devolvia "aberto" = vendia) e a
// do painel colocava "encerrado" antes de "não começou". Uma função só, um
// conjunto de casos só.

import (
	"testing"
	"time"
)

func TestWindowAt(t *testing.T) {
	now := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC)
	ptr := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	cases := []struct {
		name     string
		status   string
		startsAt *time.Time
		endsAt   *time.Time
		want     Window
	}{
		// Caminho feliz: campanha no ar.
		{"ativo sem datas", "active", nil, nil, WindowOpen},
		{"ativo dentro da janela", "active", ptr(-2 * time.Hour), ptr(2 * time.Hour), WindowOpen},
		{"comeca exatamente agora", "active", ptr(0), ptr(time.Hour), WindowOpen},
		{"termina exatamente agora ainda vende", "active", nil, ptr(0), WindowOpen},

		// Ainda não começou.
		{"agendado para daqui a pouco", "active", ptr(30 * time.Minute), nil, WindowNotStarted},
		{"agendado com status scheduled", "scheduled", ptr(time.Hour), nil, WindowNotStarted},

		// O BURACO que só o filtro status='active' das queries estava tapando:
		// ActivateScheduledEvent não tem chamador, então um evento agendado
		// nunca vira 'active' sozinho. Sem este caso, comentário numa campanha
		// cuja hora marcada já passou criaria carrinho e reservaria estoque.
		{"scheduled sem data nenhuma", "scheduled", nil, nil, WindowNotStarted},
		{"scheduled com a hora marcada ja vencida", "scheduled", ptr(-time.Hour), nil, WindowNotStarted},
		{"scheduled vencido mas com ends_at futuro", "scheduled", ptr(-time.Hour), ptr(time.Hour), WindowNotStarted},

		// Já acabou — os dois caminhos.
		{"status ended sem datas", "ended", nil, nil, WindowEnded},
		{"ends_at vencido com status ativo", "active", nil, ptr(-time.Minute), WindowEnded},
		{"ends_at vencido ha dias", "active", ptr(-7 * 24 * time.Hour), ptr(-24 * time.Hour), WindowEnded},

		// Encerrar à mão vence tudo: o lojista apertou o botão.
		{"encerrado a mao com inicio no futuro", "ended", ptr(time.Hour), nil, WindowEnded},

		// Configuração incoerente: "ainda não começou" tem precedência, porque
		// "começa em X" é mais útil ao comprador do que "acabou".
		{"agendado no futuro e ends_at no passado", "active", ptr(time.Hour), ptr(-time.Hour), WindowNotStarted},

		// Live (type single/multi) nunca passou por este gate antes — o filtro
		// status='active' das queries era a única proteção. Estes são os casos
		// que passavam batido e criavam carrinho.
		{"live encerrada por ends_at", "active", nil, ptr(-time.Second), WindowEnded},
		{"live com status ended", "ended", ptr(-time.Hour), nil, WindowEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WindowAt(tc.status, tc.startsAt, tc.endsAt, now); got != tc.want {
				t.Errorf("WindowAt = %v, quero %v", got, tc.want)
			}
		})
	}
}

// EffectiveStatus é a projeção de WindowAt no vocabulário do painel. Este teste
// existe para que o rótulo que o lojista vê e a decisão que a ingestão toma não
// possam divergir de novo.
func TestEffectiveStatusSegueWindowAt(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name        string
		status      string
		scheduledAt *time.Time
		endsAt      *time.Time
		want        string
	}{
		{"ativo", "active", nil, nil, "active"},
		{"encerrado a mao", "ended", nil, nil, "ended"},
		{"ends_at vencido", "active", nil, &past, "ended"},
		{"inicio no futuro", "active", &future, nil, "scheduled"},
		{"scheduled sem janela", "scheduled", nil, nil, "scheduled"},
		{"scheduled com hora vencida", "scheduled", &past, nil, "scheduled"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveStatus(tc.status, tc.scheduledAt, tc.endsAt); got != tc.want {
				t.Errorf("EffectiveStatus = %q, quero %q", got, tc.want)
			}
		})
	}
}

// D18 — sessão encerrada não anota pedido, mesmo com a campanha aberta. Os dois
// valores de "no ar" ('active' e 'live') precisam continuar vendendo: 'live' é
// o que StartSession grava e 'active' é o default da coluna.
func TestSessionAcceptsPurchase(t *testing.T) {
	cases := map[string]bool{
		SessionStatusActive: true,
		SessionStatusLive:   true,
		SessionStatusEnded:  false,
	}
	for status, want := range cases {
		if got := SessionAcceptsPurchase(status); got != want {
			t.Errorf("SessionAcceptsPurchase(%q) = %v, quero %v", status, got, want)
		}
	}
}
