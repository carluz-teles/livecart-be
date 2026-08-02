package order

// RN-19 — `liveSessionId` no pedido NUNCA carregou um id de sessão: o filtro
// SQL sempre foi `c.event_id = ?` e o service preenchia o campo com row.EventID
// sob um comentário que só justificava o nome do JSON. É um nome que engana
// ativamente: quem for implementar contra ele passa um id de live_sessions e
// recebe zero linhas, sem erro nenhum.
//
// O contrato novo é `eventId`/`eventTitle`. Os antigos continuam saindo com o
// MESMO valor até o frontend migrar — e é isso que este teste tranca: o dia em
// que os dois divergirem, alguma tela passa a mostrar o campo errado.

import "testing"

func TestPedidoExpoeEventIdEOAliasAntigoComOMesmoValor(t *testing.T) {
	out := OrderOutput{
		ID:         "ord-1",
		ShortID:    1001,
		EventID:    "evt-abc",
		EventTitle: "Semana Black",
	}

	resp := NewOrderResponse(out)

	if resp.EventID != "evt-abc" {
		t.Errorf("eventId = %q", resp.EventID)
	}
	if resp.EventTitle != "Semana Black" {
		t.Errorf("eventTitle = %q", resp.EventTitle)
	}
	if resp.LiveSessionID != resp.EventID {
		t.Errorf("liveSessionId (%q) divergiu de eventId (%q) — o alias tem de ser o mesmo valor",
			resp.LiveSessionID, resp.EventID)
	}
	if resp.LiveTitle != resp.EventTitle {
		t.Errorf("liveTitle (%q) divergiu de eventTitle (%q)", resp.LiveTitle, resp.EventTitle)
	}
}
