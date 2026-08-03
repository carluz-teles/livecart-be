package live

// A LIVE precisa de polling, e por uma regra diferente de post/reel.
//
// O caso real que trouxe este teste: numa transmissão ao vivo, os dois
// primeiros comentários viraram pedido e todos os seguintes sumiram — sem erro,
// sem log, sem nada no banco. A Meta entrega live_comments só "for the duration
// of the broadcast" e, na prática, para de entregar no meio. Como o webhook era
// o ÚNICO caminho de captura da live, o que ele perdeu ninguém recuperou. É a
// perda de venda mais cara possível e a mais invisível: não dá para contar o
// que nunca chegou.
//
// Post/reel desligam o polling no primeiro webhook (webhook_active) porque ali
// ele é ponte, não rede. Aplicar a MESMA regra à live reproduziria o bug ao pé
// da letra: é o primeiro webhook que chega — é do resto que se perde. Por isso
// a live ignora webhook_active e os dois caminhos andam juntos, com o dedup por
// platform_comment_id resolvendo a duplicata.

import (
	"context"
	"testing"
)

func TestLiveEntraNoPollingMesmoComWebhookAtivo(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name          string
		slug          string
		sessionType   string
		sessionStatus string
		eventStatus   string
		webhookActive bool
		want          bool
	}{
		{
			name: "live no ar entra", slug: "poll-live-ativa",
			sessionType: SessionTypeLive, sessionStatus: SessionStatusActive,
			eventStatus: "active", webhookActive: false, want: true,
		},
		{
			// O caso do bug: o webhook JÁ chegou nesta mídia. Para post isso
			// desligaria o polling; para live não pode desligar.
			name: "live com webhook ja recebido continua entrando", slug: "poll-live-webhook",
			sessionType: SessionTypeLive, sessionStatus: SessionStatusActive,
			eventStatus: "active", webhookActive: true, want: true,
		},
		{
			// Encerrar a transmissão desliga o polling: fora da janela da
			// transmissão a API não devolve comentário de live de qualquer forma.
			name: "live encerrada sai", slug: "poll-live-encerrada",
			sessionType: SessionTypeLive, sessionStatus: SessionStatusEnded,
			eventStatus: "active", webhookActive: false, want: false,
		},
		{
			name: "live de evento encerrado sai", slug: "poll-live-evento-off",
			sessionType: SessionTypeLive, sessionStatus: SessionStatusActive,
			eventStatus: "ended", webhookActive: false, want: false,
		},
		{
			// A regra de post/reel continua valendo: lá o webhook substitui.
			name: "post com webhook ativo sai", slug: "poll-post-webhook",
			sessionType: SessionTypePost, sessionStatus: SessionStatusActive,
			eventStatus: "active", webhookActive: true, want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedMedia(t, ctx, tc.slug, tc.eventStatus, tc.sessionStatus, tc.sessionType, 0)
			if tc.webhookActive {
				if _, err := testPool.Exec(ctx,
					`UPDATE live_session_platforms SET webhook_active = true WHERE session_id = $1::uuid`,
					f.sessionID,
				); err != nil {
					t.Fatalf("marcar webhook_active: %v", err)
				}
			}

			medias, err := testRepo.ListPollableMedia(ctx)
			if err != nil {
				t.Fatalf("ListPollableMedia: %v", err)
			}
			found := false
			for _, m := range medias {
				if m.MediaID == f.mediaID {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("midia %s no polling = %v, quero %v", f.mediaID, found, tc.want)
			}
		})
	}
}

// O teto de 12h: a live esquecida para de ser consultada sozinha.
//
// "O lojista esqueceu de encerrar" é o caso normal, não a exceção — quem termina
// a live fecha o Instagram e vai embora. Sem teto, essa sessão seria consultada
// a cada 20s até o EVENTO acabar, e num evento guarda-chuva isso é uma semana:
// ~30 mil chamadas à Graph por transmissão esquecida.
//
// O desligamento normal NÃO é este teto — é o sweep que encerra a sessão quando
// a transmissão sai do ar. Este teste guarda a rede que sobra quando o sweep
// não consegue rodar (token expirado, Graph fora do ar).
func TestLiveEsquecidaSaiDoPollingPeloTeto(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		slug       string
		mediaHours int
		want       bool
	}{
		{"live vinculada ha 1h continua", "poll-live-1h", 1, true},
		{"live vinculada ha 11h continua", "poll-live-11h", 11, true},
		{"live vinculada ha 13h sai", "poll-live-13h", 13, false},
		{"live vinculada ha 7 dias sai", "poll-live-7d", 24 * 7, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedMedia(t, ctx, tc.slug, "active", SessionStatusActive, SessionTypeLive, 0)
			// added_at é o carimbo do VÍNCULO da mídia, que é quando a
			// transmissão passou a ser capturada.
			if _, err := testPool.Exec(ctx,
				`UPDATE live_session_platforms
				    SET added_at = now() - make_interval(hours => $2)
				  WHERE session_id = $1::uuid`,
				f.sessionID, tc.mediaHours,
			); err != nil {
				t.Fatalf("envelhecer o vinculo: %v", err)
			}

			medias, err := testRepo.ListPollableMedia(ctx)
			if err != nil {
				t.Fatalf("ListPollableMedia: %v", err)
			}
			found := false
			for _, m := range medias {
				if m.MediaID == f.mediaID {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("live vinculada ha %dh no polling = %v, quero %v", tc.mediaHours, found, tc.want)
			}
		})
	}
}
