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
//  4. status 'scheduled' SEM instante de início é "não começou".
//
// A regra 4 já foi mais larga: barrava TODO evento 'scheduled', inclusive o de
// hora marcada já vencida. Isso era necessário enquanto nada ativava o evento —
// ActivateScheduledEvent não tinha chamador, então um evento agendado nunca
// virava 'active' e teria vendido para sempre com a hora vencida. O preço é que
// ele também nunca vendia: o ciclo de vida completo de um evento agendado era
// "não começou" → "encerrado" (E37).
//
// Com a ativação no ar (Service.Start pelo botão do lojista e
// SweepScheduledEventsReadyToStart pelo horário), quem decide é a JANELA, não o
// rótulo: starts_at no futuro já barra pela regra 2, e o status 'scheduled' que
// sobrevive entre a hora marcada e o tick do sweep não deve mudar a resposta ao
// comprador — se a janela abriu, a campanha vende.
//
// O resíduo da regra 4 é o caso sem instante nenhum: 'scheduled' sem starts_at
// (nem scheduled_at, que é o que a leitura preenche aqui) é um evento que
// declarou ter hora marcada e não diz qual. Não há como afirmar que começou, e
// errar para "aberto" venderia antes da hora sem nada segurando. Não deveria
// existir — Create só grava 'scheduled' quando recebe a data —, mas é barato.
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
	if storedStatus == "scheduled" && startsAt == nil {
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
