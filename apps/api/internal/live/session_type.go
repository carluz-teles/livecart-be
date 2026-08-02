package live

import "strconv"

// D3 — o tipo da transmissão passa a viver em live_sessions.type, com o CHECK
// que live_events.type nunca teve. Enquanto a 000119 (contract) não roda,
// live_events.type continua sendo escrito no vocabulário ANTIGO, porque o
// frontend ainda lê event.type e só conhece single|multi|post|story.
//
// Estes dois mapas são o único ponto de tradução entre os dois vocabulários.
// Duplicá-los é o caminho mais curto para o CHECK estourar como 500 no INSERT
// (a lição E6 da errata: validação de aplicação desalinhada do CHECK).

// Status de sessão. 'active' e 'live' são ambos "no ar" — 'live' é o que
// StartSession grava; 'active' é o default da coluna.
const (
	SessionStatusActive = "active"
	SessionStatusLive   = "live"
	SessionStatusEnded  = "ended"
)

const (
	SessionTypeLive  = "live"
	SessionTypePost  = "post"
	SessionTypeReel  = "reel"
	SessionTypeStory = "story"
)

// SessionTypeFromEventType traduz o vocabulário do evento para o da sessão.
// 'single' e 'multi' descrevem quantas plataformas a live usava — as duas são
// live. Um valor já no vocabulário novo passa direto, e é assim que 'reel'
// entra no sistema.
func SessionTypeFromEventType(eventType string) string {
	switch eventType {
	case SessionTypePost, SessionTypeReel, SessionTypeStory:
		return eventType
	default:
		return SessionTypeLive
	}
}

// LegacyEventType devolve o valor a gravar em live_events.type. Só 'reel'
// precisa de tradução: ele não existe no vocabulário antigo e o frontend
// quebraria ao receber um type desconhecido, então um Reel continua sendo
// rotulado 'post' no evento — a distinção real vive na sessão.
func LegacyEventType(eventType string) string {
	if eventType == SessionTypeReel {
		return SessionTypePost
	}
	return eventType
}

// IsValidSessionType espelha o CHECK live_sessions_type_check da 000109.
func IsValidSessionType(t string) bool {
	switch t {
	case SessionTypeLive, SessionTypePost, SessionTypeReel, SessionTypeStory:
		return true
	default:
		return false
	}
}

// SessionLabel é o nome da transmissão como o COMPRADOR a vê — a variável
// {sessao} das mensagens automáticas. A sessão não tem título próprio no
// schema (o título é do evento), então o rótulo é derivado do tipo mais a
// ordem dentro da campanha: "Live 1", "Post 2", "Story 3".
//
// Fica no domínio, e não montado na hora do envio, porque o mesmo rótulo vai
// aparecer no painel: duas construções do nome dariam "Live 2" numa tela e
// "live #2" na DM do comprador.
func SessionLabel(sessionType string, sequenceOrder int) string {
	var name string
	switch sessionType {
	case SessionTypePost:
		name = "Post"
	case SessionTypeReel:
		name = "Reel"
	case SessionTypeStory:
		name = "Story"
	default:
		name = "Live"
	}
	if sequenceOrder <= 0 {
		// Sessão sem ordem definida (dado antigo): "a Live" ainda é uma frase
		// correta, "a Live 0" não.
		return name
	}
	return name + " " + strconv.Itoa(sequenceOrder)
}

// IsPostCommerceSessionType reporta se a sessão segue as regras de
// post-commerce (whitelist da promoção, auto-add de produto único, resposta
// privada). Post, Reel e Story compartilham as regras; só o canal de resposta
// muda. Esquecer 'reel' aqui faria todo Reel perder as regras em silêncio.
func IsPostCommerceSessionType(t string) bool {
	return t == SessionTypePost || t == SessionTypeReel || t == SessionTypeStory
}

// MetricCutoverSessionAttribution é a chave do marcador gravado pela 000119.
// Uma constante e não a string solta: o valor tem de casar EXATAMENTE com o do
// INSERT da migration, e um typo aqui não dá erro nenhum — só faz o marcador
// sumir da resposta, que é o modo de falha que ele existe para evitar.
const MetricCutoverSessionAttribution = "session_attribution"
