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
