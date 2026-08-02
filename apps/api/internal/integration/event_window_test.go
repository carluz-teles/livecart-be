package integration

import (
	"testing"
	"time"
)

func TestEventWindowState(t *testing.T) {
	now := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC)
	ptr := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	cases := []struct {
		name        string
		status      string
		scheduledAt *time.Time
		endsAt      *time.Time
		want        eventWindow
	}{
		// Caminho feliz: campanha no ar.
		{"ativo sem datas", "active", nil, nil, windowOpen},
		{"ativo dentro da janela", "active", ptr(-2 * time.Hour), ptr(2 * time.Hour), windowOpen},
		{"comeca exatamente agora", "active", ptr(0), ptr(time.Hour), windowOpen},
		{"termina exatamente agora ainda vende", "active", nil, ptr(0), windowOpen},

		// Ainda não começou.
		{"agendado para daqui a pouco", "active", ptr(30 * time.Minute), nil, windowNotStarted},
		{"agendado com status scheduled", "scheduled", ptr(time.Hour), nil, windowNotStarted},

		// Já acabou — os dois caminhos.
		{"status ended sem datas", "ended", nil, nil, windowEnded},
		{"ends_at vencido com status ativo", "active", nil, ptr(-time.Minute), windowEnded},
		{"ends_at vencido ha dias", "active", ptr(-7 * 24 * time.Hour), ptr(-24 * time.Hour), windowEnded},

		// Configuração incoerente: "ainda não começou" tem precedência, porque
		// "começa em X" é mais útil ao comprador do que "acabou".
		{"agendado no futuro e ends_at no passado", "active", ptr(time.Hour), ptr(-time.Hour), windowNotStarted},

		// Live (type single/multi) nunca passou por este gate antes — o filtro
		// status='active' das queries era a única proteção. Estes são os casos
		// que passavam batido e criavam carrinho.
		{"live encerrada por ends_at", "active", nil, ptr(-time.Second), windowEnded},
		{"live com status ended", "ended", ptr(-time.Hour), nil, windowEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eventWindowState(tc.status, tc.scheduledAt, tc.endsAt, now)
			if got != tc.want {
				t.Errorf("eventWindowState = %v, quero %v", got, tc.want)
			}
		})
	}
}

// O gate só dispara com intenção de compra — mas quando dispara, a resposta
// depende de scheduledAt não ser nil no ramo windowNotStarted (o handler
// desreferencia o ponteiro). Este teste trava esse acoplamento: windowNotStarted
// só pode ser devolvido quando scheduledAt existe.
func TestWindowNotStartedImplicaScheduledAtPresente(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name        string
		status      string
		scheduledAt *time.Time
		endsAt      *time.Time
	}{
		{"sem datas", "active", nil, nil},
		{"so ends_at futuro", "active", nil, &future},
		{"so ends_at passado", "active", nil, &past},
		{"status ended sem scheduled", "ended", nil, nil},
		{"scheduled no passado", "active", &past, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventWindowState(tc.status, tc.scheduledAt, tc.endsAt, now); got == windowNotStarted && tc.scheduledAt == nil {
				t.Error("windowNotStarted com scheduledAt nil — o handler faria panic ao desreferenciar")
			}
		})
	}
}
