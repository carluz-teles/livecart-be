package live

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// =============================================================================
// MÍDIA DE UMA SESSÃO — vincular a publicação DEPOIS que a transmissão existe
//
// Por que esta rota existe: a campanha guarda-chuva se apoia em "marco a Semana
// Black hoje e penduro a live de segunda quando ela existir". Criar a sessão
// sem mídia já era possível (CreateSessionRequest deixou platform/platformLiveId
// opcionais), mas o "depois" não tinha caminho: a única rota de vínculo era
// POST /lives/:id/platforms, que resolve a sessão sozinha
// (GetActiveSessionByEvent) e por isso não serve a uma campanha com mais de uma
// transmissão — ela escreveria na sessão errada sem avisar. O painel a expõe só
// como "Crash recovery" e apenas para live.
//
// Resultado: a sessão sem mídia nascia num beco. O painel mostrava o badge
// "Sem publicação vinculada" e a ajuda prometia "vincule quando a publicação
// existir", e não havia como. Esta rota é a que fecha a promessa, ancorada na
// SESSÃO — a unidade que de fato tem mídia.
// =============================================================================

// LinkSessionMediaRequest é o corpo de POST /lives/:id/sessions/:sessionId/platforms.
type LinkSessionMediaRequest struct {
	// Platform é opcional no corpo porque só há um valor possível hoje; vazio
	// vira "instagram" no ToInput. PlatformLiveID é o que importa: sem ele não
	// há vínculo nenhum, e essa é a diferença desta rota para CreateSession,
	// onde a ausência dos dois é um caminho legítimo.
	Platform       string `json:"platform"`
	PlatformLiveID string `json:"platformLiveId"`
	// Metadados da publicação (permalink/capa/legenda), gravados na MÍDIA como
	// em CreatePostEvent e CreateSession. Sem eles a MESMA publicação ficaria
	// rica quando entra na criação e pobre quando entra pelo vínculo posterior.
	MediaPermalink    string `json:"mediaPermalink"`
	MediaThumbnailURL string `json:"mediaThumbnailUrl"`
	MediaCaption      string `json:"mediaCaption"`
}

func (r LinkSessionMediaRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Platform, validation.In("instagram")),
		validation.Field(&r.PlatformLiveID, validation.Required, validation.Length(1, 100)),
	)
}

// ToInput traduz o Request no input do usecase. O evento e a sessão vêm do
// path — a posse é checada no service, nunca no corpo.
func (r LinkSessionMediaRequest) ToInput(storeID, eventID, sessionID string) LinkSessionMediaInput {
	platform := r.Platform
	if platform == "" {
		platform = "instagram"
	}
	return LinkSessionMediaInput{
		StoreID:           storeID,
		EventID:           eventID,
		SessionID:         sessionID,
		Platform:          platform,
		PlatformLiveID:    r.PlatformLiveID,
		MediaPermalink:    r.MediaPermalink,
		MediaThumbnailURL: r.MediaThumbnailURL,
		MediaCaption:      r.MediaCaption,
	}
}

// LinkSessionMediaInput é o input do usecase de vínculo de mídia.
type LinkSessionMediaInput struct {
	StoreID           string
	EventID           string
	SessionID         string
	Platform          string
	PlatformLiveID    string
	MediaPermalink    string
	MediaThumbnailURL string
	MediaCaption      string
}
