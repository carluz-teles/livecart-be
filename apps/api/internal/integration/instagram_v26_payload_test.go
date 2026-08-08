package integration

// O contrato do webhook de live_comments na v26 da Graph API.
//
// A entrega de comentário de live vinha falhando de forma inexplicável — numa
// janela de oito minutos com a live no ar, 1 de 23 comentários chegou por
// webhook enquanto os `messaging` fluíam normalmente. Depois da atualização
// para a v26 a entrega voltou: 178 webhooks de live_comments numa janela
// equivalente.
//
// Este teste congela a FORMA do payload dessa versão. Se ela mudar de novo, o
// sintoma seria o mesmo de antes — comentário chegando e sendo descartado em
// silêncio, indistinguível de "a Meta parou de entregar".
//
// O campo mais perigoso é o id do comentário: ele vem em `id`, e não em
// `comment_id`. Ler a chave errada devolve string vazia, e uma string vazia
// atravessa o fluxo inteiro sem erro — o dedup por comment_id passa a casar
// tudo com tudo.

import (
	"encoding/json"
	"testing"
)

// payloadV26 é exatamente o exemplo publicado pela Meta para a v26.
const payloadV26 = `{
  "field": "live_comments",
  "value": {
    "from": {
      "id": "232323232",
      "username": "test",
      "self_ig_scoped_id": "232323232"
    },
    "media": {
      "id": "123123123",
      "media_product_type": "LIVE"
    },
    "id": "17865799348089039",
    "text": "This is an example."
  }
}`

func TestPayloadV26DeLiveCommentEhLidoPorInteiro(t *testing.T) {
	var envelope struct {
		Field string                    `json:"field"`
		Value InstagramLiveCommentValue `json:"value"`
	}
	if err := json.Unmarshal([]byte(payloadV26), &envelope); err != nil {
		t.Fatalf("o payload da v26 não desserializa: %v", err)
	}
	v := envelope.Value

	if envelope.Field != "live_comments" {
		t.Errorf("field = %q, quero live_comments", envelope.Field)
	}
	// O campo que quebra em silêncio se lido errado.
	if v.CommentID != "17865799348089039" {
		t.Errorf("comment id = %q, quero 17865799348089039 — na v26 ele vem em `id`, não em `comment_id`, "+
			"e vazio aqui faz o dedup casar comentários distintos entre si", v.CommentID)
	}
	if v.Text != "This is an example." {
		t.Errorf("text = %q", v.Text)
	}
	if v.From.Username != "test" {
		t.Errorf("username = %q", v.From.Username)
	}
	if v.From.ID != "232323232" {
		t.Errorf("from.id = %q", v.From.ID)
	}
	// O IGSID é o único id aceito como destinatário de DM.
	if v.From.SelfIGScopedID != "232323232" {
		t.Errorf("self_ig_scoped_id = %q — sem ele o comprador fica inalcançável por DM", v.From.SelfIGScopedID)
	}
	if v.Media.ID != "123123123" {
		t.Errorf("media.id = %q — é por ele que o comentário encontra o evento", v.Media.ID)
	}
}

// A v26 acrescentou media_product_type. Não decidimos nada com ele hoje, mas se
// um dia decidirmos, tem de estar sendo lido — e se não estiver, este teste diz
// onde acrescentar.
func TestMediaProductTypeDaV26ChegaAoDominio(t *testing.T) {
	var envelope struct {
		Value InstagramLiveCommentValue `json:"value"`
	}
	if err := json.Unmarshal([]byte(payloadV26), &envelope); err != nil {
		t.Fatalf("desserializando: %v", err)
	}
	if envelope.Value.Media.MediaProductType != "LIVE" {
		t.Errorf("media_product_type = %q, quero LIVE — a v26 passou a mandar este campo e ele distingue "+
			"comentário de live de comentário de post", envelope.Value.Media.MediaProductType)
	}
}

// Sem o carimbo do próprio comentário, CommentedAt cai no tempo da ENTREGA. O
// payload da v26 não traz timestamp, então este fallback é o caminho normal —
// e ele decide se o private reply ainda sai (janela de 7 dias, RN-37).
func TestCommentedAtCaiNoTempoDaEntregaQuandoAV26NaoMandaCarimbo(t *testing.T) {
	var envelope struct {
		Value InstagramLiveCommentValue `json:"value"`
	}
	if err := json.Unmarshal([]byte(payloadV26), &envelope); err != nil {
		t.Fatalf("desserializando: %v", err)
	}
	const entrega = int64(1786217024)
	if got := envelope.Value.CommentedAt(entrega); got != entrega {
		t.Errorf("CommentedAt = %d, quero %d (o carimbo da entrega)", got, entrega)
	}
	// E sem nenhuma fonte, zero — o chamador erra para o lado de tentar responder.
	if got := envelope.Value.CommentedAt(0); got != 0 {
		t.Errorf("CommentedAt sem fonte alguma = %d, quero 0", got)
	}
}
