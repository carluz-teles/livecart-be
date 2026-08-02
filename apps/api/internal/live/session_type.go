package live

// D3 — o tipo da transmissão passa a viver em live_sessions.type, com o CHECK
// que live_events.type nunca teve. Enquanto a 000119 (contract) não roda,
// live_events.type continua sendo escrito no vocabulário ANTIGO, porque o
// frontend ainda lê event.type e só conhece single|multi|post|story.
//
// Estes dois mapas são o único ponto de tradução entre os dois vocabulários.
// Duplicá-los é o caminho mais curto para o CHECK estourar como 500 no INSERT
// (a lição E6 da errata: validação de aplicação desalinhada do CHECK).

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

// IsPostCommerceSessionType reporta se a sessão segue as regras de
// post-commerce (whitelist da promoção, auto-add de produto único, resposta
// privada). Post, Reel e Story compartilham as regras; só o canal de resposta
// muda. Esquecer 'reel' aqui faria todo Reel perder as regras em silêncio.
func IsPostCommerceSessionType(t string) bool {
	return t == SessionTypePost || t == SessionTypeReel || t == SessionTypeStory
}
