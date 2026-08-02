package integration

import (
	"encoding/json"
	"strings"
)

// InstagramWebhookPayload represents the root payload sent by Meta webhooks
type InstagramWebhookPayload struct {
	Object string           `json:"object"`
	Entry  []InstagramEntry `json:"entry"`
}

// InstagramEntry represents a single entry in the webhook payload.
//
// Time é o carimbo da ENTREGA do webhook, não do comentário — e a Meta o manda
// em MILISSEGUNDOS nos produtos de Instagram. Não use direto: passe por
// epochSeconds, e prefira o carimbo do próprio comentário quando ele vier
// (InstagramLiveCommentValue.CommentedAt).
type InstagramEntry struct {
	ID        string             `json:"id"`
	Time      int64              `json:"time"`
	Changes   []InstagramChange  `json:"changes,omitempty"`
	Messaging []InstagramMessage `json:"messaging,omitempty"`
}

// msEpochThreshold separa segundos de milissegundos. 1e11 segundos é o ano
// 5138 e 1e11 milissegundos é março de 1973: qualquer carimbo real de hoje cai
// do lado certo por várias ordens de grandeza.
const msEpochThreshold = int64(100_000_000_000)

// epochSeconds normaliza um carimbo unix que pode chegar em segundos ou em
// milissegundos, devolvendo SEMPRE segundos. 0 significa "não sei" — os
// chamadores erram para o lado de tentar enviar.
//
// Sem isto, um carimbo em ms vira time.Unix(1.75e12, 0) ≈ ano 57000: qualquer
// comparação de idade dá "acabou de ser postado" e a janela de resposta tardia
// nunca é aplicada no caminho do webhook.
func epochSeconds(ts int64) int64 {
	if ts <= 0 {
		return 0
	}
	if ts >= msEpochThreshold {
		return ts / 1000
	}
	return ts
}

// commentTime aceita o carimbo do comentário nas DUAS formas em que a Meta o
// entrega: número unix (em segundos ou milissegundos, conforme o produto) e
// string ISO8601 do Graph. Sem isso, um `created_time` textual derrubaria o
// unmarshal do payload inteiro — e com ele o comentário, não só o carimbo.
type commentTime int64

func (t *commentTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*t = commentTime(parseGraphTimestamp(v))
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		// Carimbo ilegível não invalida o comentário: fica "não sei".
		return nil
	}
	*t = commentTime(n)
	return nil
}

// InstagramChange represents a change event (used for live_comments)
type InstagramChange struct {
	Field string      `json:"field"`
	Value interface{} `json:"value"`
}

// InstagramLiveCommentValue represents the value for live_comments field
type InstagramLiveCommentValue struct {
	From      InstagramUser  `json:"from"`
	CommentID string         `json:"id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Text      string         `json:"text"`
	Media     InstagramMedia `json:"media"`
	// Carimbo do PRÓPRIO comentário. Os dois nomes existem porque a Meta não
	// é consistente entre produtos (e alguns não mandam nenhum) — por isso é
	// fallback, não fonte única. Ver CommentedAt.
	Timestamp   commentTime `json:"timestamp,omitempty"`
	CreatedTime commentTime `json:"created_time,omitempty"`
}

// CommentedAt devolve quando o comentário foi escrito, em unix SECONDS, com a
// melhor fonte disponível: o carimbo do próprio comentário quando ele vier, e
// só então o da entrega do webhook (entryTime). Devolve 0 quando não há
// nenhuma — o chamador erra para o lado de tentar responder.
//
// A distinção importa porque entry.time é quando a Meta ENTREGOU o evento: numa
// redelivery ou num backlog de fila ele pode ser dias mais novo que o
// comentário, e é a idade do COMENTÁRIO que decide se o private reply ainda
// sai (RN-37, 7 dias).
func (v InstagramLiveCommentValue) CommentedAt(entryTime int64) int64 {
	for _, ts := range []int64{int64(v.Timestamp), int64(v.CreatedTime), entryTime} {
		if s := epochSeconds(ts); s > 0 {
			return s
		}
	}
	return 0
}

// InstagramUser represents an Instagram user
type InstagramUser struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	SelfIGScopedID string `json:"self_ig_scoped_id"`
}

// InstagramMedia represents media information
type InstagramMedia struct {
	ID               string `json:"id"`
	MediaProductType string `json:"media_product_type"`
}

// InstagramMessage represents a direct message in the webhook.
//
// Timestamp vem em MILISSEGUNDOS na Messaging API. Passe por epochSeconds antes
// de comparar com qualquer coisa.
type InstagramMessage struct {
	Sender    InstagramIDOnly         `json:"sender"`
	Recipient InstagramIDOnly         `json:"recipient"`
	Timestamp int64                   `json:"timestamp"`
	Message   InstagramMessageContent `json:"message"`
}

// InstagramIDOnly represents an object with just an ID
type InstagramIDOnly struct {
	ID string `json:"id"`
}

// InstagramMessageContent represents the content of a direct message
type InstagramMessageContent struct {
	MID     string                 `json:"mid"`
	Text    string                 `json:"text"`
	IsEcho  bool                   `json:"is_echo,omitempty"`
	ReplyTo *InstagramMessageReply `json:"reply_to,omitempty"`
}

// InstagramMessageReply carries the context a DM is replying to. For story
// replies it holds the story the buyer tapped reply on — that story id maps to
// our published story-commerce event.
type InstagramMessageReply struct {
	MID   string               `json:"mid,omitempty"`
	Story *InstagramReplyStory `json:"story,omitempty"`
}

// InstagramReplyStory is the story a DM is replying to.
type InstagramReplyStory struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`
}

// ProcessInstagramCommentInput represents input for processing a live comment
type ProcessInstagramCommentInput struct {
	AccountID string
	MediaID   string
	CommentID string
	UserID    string
	Username  string
	Text      string
	// Timestamp é quando o COMENTÁRIO foi escrito, em unix SECONDS. 0 = não
	// sabemos. Quem preenche normaliza (ms vem do webhook, ISO8601 vem do
	// Graph): daqui para baixo ninguém mais converte nada.
	Timestamp int64
	// Channel is the reply channel: "" / "comment" (default) replies on the
	// comment thread with a DM fallback; "dm" replies straight via DM — used by
	// story replies, which arrive as DMs and have no public comment to answer.
	Channel    string
	RawPayload []byte // Original webhook payload for audit storage
	// SignatureValid é o RESULTADO REAL do X-Hub-Signature-256 desta requisição,
	// e viaja colado ao RawPayload de propósito: os dois alimentam a MESMA linha
	// de webhook_events, e uma linha que grava "assinatura válida" sobre um
	// payload cuja checagem falhou é pior que linha nenhuma.
	//
	// É esta coluna que decide o deploy 2 do modo observação (§5.2, ordem 9): o
	// 401 só entra depois de dias de tráfego 100% válido. Com o `true` fixo que
	// vivia aqui, a consulta respondia 100% válido POR CONSTRUÇÃO — inclusive
	// para as requisições que o handler acabara de reprovar e aceitar mesmo
	// assim. Quem lesse o painel ligaria a exigência e derrubaria a captura de
	// comentários da base inteira, que é exatamente o desastre que o modo
	// observação existe para evitar.
	//
	// Falso por padrão sem consequência: o polling não tem assinatura e também
	// não tem RawPayload, então nem chega a gravar auditoria.
	SignatureValid bool
}

// ProcessInstagramMessageInput represents input for processing a DM
type ProcessInstagramMessageInput struct {
	AccountID string
	SenderID  string
	MessageID string
	Text      string
	Timestamp int64
	// ReplyToStoryID is set when the DM is a reply to a story (reply_to.story.id),
	// mapping it to a published story-commerce event.
	ReplyToStoryID string
	// IsEcho is true for echoes of our own outbound messages — skip those.
	IsEcho     bool
	RawPayload []byte // Original webhook payload for audit storage
	// SignatureValid — mesma razão de ProcessInstagramCommentInput. O DM tem
	// caminho de auditoria próprio (EventType "messaging") e gravava o mesmo
	// `true` fixo.
	SignatureValid bool
}
