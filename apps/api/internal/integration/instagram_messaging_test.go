package integration

// O array `messaging` do webhook não carrega só mensagem.
//
// A Meta manda recibo de leitura, reação e confirmação de entrega no MESMO
// array, e esses eventos não trazem o objeto `message`. O unmarshal preenche o
// zero value e o evento seguia como se fosse uma DM de texto vazio: cada DM
// enviada gerava, segundos depois, um "no integration found for instagram
// account" no log — que era só o comprador ABRINDO a mensagem.
//
// O ruído não era o pior: ele aparecia igual a uma falha real de resolução de
// loja, então a falha real ficava indistinguível do comprador lendo a DM.

import (
	"encoding/json"
	"testing"
)

// O payload de um recibo de leitura, como a Meta entrega: entry.messaging com
// sender/recipient/timestamp e NENHUM objeto `message`.
const readReceiptPayload = `{
  "object": "instagram",
  "entry": [{
    "id": "17841439350112281",
    "time": 1785735000000,
    "messaging": [{
      "sender": {"id": "2715070248858106"},
      "recipient": {"id": "17841439350112281"},
      "timestamp": 1785735000000,
      "read": {"mid": "aWdfZAG1faXRlbToxOk"}
    }]
  }]
}`

// Uma DM de verdade, para o discriminador não recusar o que importa.
const realMessagePayload = `{
  "object": "instagram",
  "entry": [{
    "id": "17841439350112281",
    "time": 1785735000000,
    "messaging": [{
      "sender": {"id": "2715070248858106"},
      "recipient": {"id": "17841439350112281"},
      "timestamp": 1785735000000,
      "message": {"mid": "mid.abc123", "text": "quero o 1003"}
    }]
  }]
}`

func TestReadReceiptHasNoMessageID(t *testing.T) {
	var payload InstagramWebhookPayload
	if err := json.Unmarshal([]byte(readReceiptPayload), &payload); err != nil {
		t.Fatalf("unmarshal do recibo de leitura: %v", err)
	}
	if len(payload.Entry) != 1 || len(payload.Entry[0].Messaging) != 1 {
		t.Fatalf("payload não tem 1 entry com 1 messaging: %+v", payload)
	}

	msg := payload.Entry[0].Messaging[0]
	// É este o teste que o handler faz para descartar. Se a Meta um dia mandar
	// mid num recibo, ou se alguém trocar o discriminador, o descarte para de
	// funcionar e o ruído volta.
	if msg.Message.MID != "" {
		t.Errorf("recibo de leitura trouxe mid %q — o discriminador do handler deixaria passar", msg.Message.MID)
	}
	if msg.Message.Text != "" {
		t.Errorf("recibo de leitura trouxe texto %q", msg.Message.Text)
	}
	if msg.Sender.ID == "" {
		t.Error("recibo sem sender: o payload de teste não representa o evento real")
	}
}

func TestRealMessageKeepsItsMessageID(t *testing.T) {
	var payload InstagramWebhookPayload
	if err := json.Unmarshal([]byte(realMessagePayload), &payload); err != nil {
		t.Fatalf("unmarshal da DM: %v", err)
	}

	msg := payload.Entry[0].Messaging[0]
	if msg.Message.MID == "" {
		t.Fatal("DM de verdade ficou sem mid — o handler descartaria a mensagem do comprador")
	}
	if msg.Message.Text != "quero o 1003" {
		t.Errorf("texto veio %q", msg.Message.Text)
	}
}
