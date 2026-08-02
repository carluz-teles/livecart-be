package integration

import "time"

// eventWindow classifica a janela comercial de um evento num instante.
//
// Existe como função pura porque essa decisão é o único freio contra vender
// fora da janela depois que o filtro de status sair das queries de resolução
// (RN-20) — e porque hoje ela só rodava para post/story, deixando live sem
// checagem nenhuma.
type eventWindow int

const (
	// windowOpen — pode vender.
	windowOpen eventWindow = iota
	// windowNotStarted — a campanha ainda vai começar.
	windowNotStarted
	// windowEnded — a campanha acabou, por status ou por ends_at vencido.
	windowEnded
)

// eventWindowState decide se o evento aceita compra agora.
//
// A ordem importa: "ainda não começou" vence "já acabou". Um evento com
// scheduled_at no futuro e ends_at no passado é configuração incoerente, e
// responder "começa em X" é mais útil ao comprador do que "acabou".
func eventWindowState(status string, scheduledAt, endsAt *time.Time, now time.Time) eventWindow {
	if scheduledAt != nil && now.Before(*scheduledAt) {
		return windowNotStarted
	}
	if status == "ended" || (endsAt != nil && now.After(*endsAt)) {
		return windowEnded
	}
	return windowOpen
}
