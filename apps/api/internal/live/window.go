package live

import (
	"fmt"
	"time"
)

// brtLocation is Brazil's current civil time (UTC-03:00). Brazil no longer
// observes DST, so a fixed offset is correct and avoids a tzdata dependency.
// Timestamps are stored in UTC; only customer-facing messages are formatted here.
var brtLocation = time.FixedZone("BRT", -3*60*60)

// Window classifica a janela comercial de um evento num instante.
type Window int

const (
	// WindowOpen — a campanha aceita compra agora.
	WindowOpen Window = iota
	// WindowNotStarted — a campanha ainda vai começar.
	WindowNotStarted
	// WindowEnded — a campanha acabou, por status ou por ends_at vencido.
	WindowEnded
)

// WindowAt é a ÚNICA decisão de "esta campanha está aberta?" do sistema.
//
// Antes existiam duas: esta (para o painel, como EffectiveStatus) e
// integration.eventWindowState (para a ingestão), com precedências e conjuntos
// de casos DIFERENTES — o painel podia rotular "agendado" a mesma linha que a
// ingestão tratava como aberta e vendia. Duas fontes da verdade sobre o mesmo
// fato é bug de design; a decisão mora no domínio e a ingestão consome daqui.
//
// A precedência, nesta ordem e por estes motivos:
//  1. encerrado à mão vence tudo — o lojista apertou o botão;
//  2. "ainda não começou" vence "já acabou", porque um evento com início no
//     futuro e fim no passado é configuração incoerente e responder
//     "começa em X" é mais útil ao comprador do que "acabou";
//  3. ends_at vencido encerra;
//  4. status 'scheduled' sem janela futura AINDA é "não começou". Este é o
//     caso que a ingestão não cobria e que só o filtro status='active' das
//     queries de resolução estava segurando: ActivateScheduledEvent não tem
//     chamador nenhum, então um evento agendado NUNCA vira 'active' sozinho e
//     ficaria vendendo para sempre com a hora marcada já vencida.
func WindowAt(storedStatus string, startsAt, endsAt *time.Time, now time.Time) Window {
	if storedStatus == "ended" {
		return WindowEnded
	}
	if startsAt != nil && now.Before(*startsAt) {
		return WindowNotStarted
	}
	if endsAt != nil && now.After(*endsAt) {
		return WindowEnded
	}
	if storedStatus == "scheduled" {
		return WindowNotStarted
	}
	return WindowOpen
}

// SessionAcceptsPurchase reporta se ESTA transmissão ainda anota pedido.
//
// D18: sessão encerrada dentro de evento aberto não vende — e é o único dos
// três casos fora-de-janela em que a campanha continua viva, então a resposta
// ao comprador é um redirecionamento, não uma negativa. Até agora quem barrava
// era o filtro ls.status IN ('active','live') da query de resolução; com ele
// fora, a decisão precisa existir aqui.
func SessionAcceptsPurchase(sessionStatus string) bool {
	return sessionStatus != SessionStatusEnded
}

// EffectiveStatus derives the status a merchant/customer should see from the
// stored status and the scheduled window, without any background job. É a
// projeção de WindowAt no vocabulário que o painel e o frontend já falam.
func EffectiveStatus(stored string, scheduledAt, endsAt *time.Time) string {
	switch WindowAt(stored, scheduledAt, endsAt, time.Now()) {
	case WindowEnded:
		return "ended"
	case WindowNotStarted:
		return "scheduled"
	default:
		return "active"
	}
}

// FormatBRT renders a UTC time in Brazil time (UTC-3) for customer messages,
// e.g. "04/06 às 18h30".
func FormatBRT(t time.Time) string {
	local := t.In(brtLocation)
	return fmt.Sprintf("%02d/%02d às %02dh%02d",
		local.Day(), int(local.Month()), local.Hour(), local.Minute())
}
