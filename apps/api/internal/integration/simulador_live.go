package integration

// O simulador de live — SÓ EM STAGING.
//
// Em staging não há como fazer o Instagram transmitir uma live e mandar
// webhook: a conta de teste não tem público, a Meta não entrega evento de
// transmissão sob demanda, e sem comentário chegando não há como exercitar o
// caminho que decide tudo — comentário vira item, item vira pedido no ERP.
//
// Este arquivo constrói o payload que a Meta mandaria e o entrega ao MESMO
// `processInstagramChange` que o webhook real chama. Não existe caminho
// paralelo: se o simulador funciona e a produção não, a diferença está na
// entrega do webhook, nunca no processamento — que é exatamente o que se quer
// poder afirmar.
//
// ═══ POR QUE A SEGURANÇA NÃO É A TELA ═══
//
// Esconder o botão no front não protege nada: a rota continuaria lá para quem
// souber o caminho, e um POST forjaria comentário — logo, carrinho, logo pedido
// no ERP de um lojista real. Por isso a proteção é em três camadas, e a
// primeira é a que importa:
//
//  1. As rotas NÃO SÃO REGISTRADAS fora de staging. Não há flag para ligar,
//     nem header para enganar — o código de registro sai pelo `return` antes de
//     montar qualquer coisa.
//
//     Medido com o servidor real em APP_ENV=production: as rotas do simulador
//     respondem 401, exatamente como `/stores/{id}/rota-que-nao-existe-jamais`.
//     Não é 404 porque a autenticação do grupo /stores roda antes do casamento
//     de rota — e o efeito é ainda melhor do que o pretendido: de fora, o
//     simulador é indistinguível de código que nunca foi escrito. Quem varrer
//     rotas não descobre sequer que a funcionalidade existe.
//  2. Cada handler reconfere `config.IsStaging()`. Redundante de propósito —
//     um dia alguém move a chamada de registro para o lugar errado, e a
//     redundância é o que transforma esse erro num 403 em vez de num incidente.
//  3. As rotas moram sob o grupo autenticado por loja, então valem as mesmas
//     regras de sessão e de posse da loja de qualquer outro endpoint.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// RegisterSimulatorRoutes monta o simulador — e só monta em staging.
//
// A checagem vive AQUI, no registro, e não dentro dos handlers: o que não é
// registrado não existe, e um endpoint que não existe não pode ser chamado por
// engano, por script, nem por alguém varrendo rotas. Os handlers reconferem por
// cima disso.
func (h *WebhookHandler) RegisterSimulatorRoutes(router fiber.Router) {
	if !config.IsStaging() {
		return
	}
	g := router.Group("/simulador/live", h.somenteStaging)
	g.Post("/evento", h.SimularEvento)
	g.Get("/sessoes", h.SimularListarSessoes)
	g.Post("/midia", h.SimularMidia)
	g.Delete("/midia/:mediaId", h.SimularEncerrarMidia)
	g.Post("/comentario", h.SimularComentario)
}

// somenteStaging é a segunda camada. Ver a nota no topo do arquivo.
func (h *WebhookHandler) somenteStaging(c *fiber.Ctx) error {
	if !config.IsStaging() {
		return httpx.DomainError(403, httpx.CodeStagingOnly, "o simulador de live existe apenas em staging")
	}
	return c.Next()
}

// SimularEventoRequest cria a campanha e a transmissão de uma vez.
type SimularEventoRequest struct {
	Titulo string `json:"titulo"`
	// DiasAteFechar é o teto da campanha. 7 quando ausente.
	DiasAteFechar int `json:"diasAteFechar"`
}

// Validate: título curto e prazo sóbrio. O teto existe porque evento é
// carrinho sem prazo enquanto está aberto, e um evento de um ano em staging
// deixaria estoque reservado até alguém lembrar.
func (r SimularEventoRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Titulo, validation.Length(0, 120)),
		validation.Field(&r.DiasAteFechar, validation.Min(0), validation.Max(60)),
	)
}

// SimularEvento cria evento + sessão sem passar pelo Instagram.
//
// Existe porque o simulador nasceu inútil sem isto: a tela só cria transmissão
// escolhendo uma live ativa da conta do Instagram, e em staging não há conta com
// live. O caminho do serviço, porém, SEMPRE aceitou sessão sem plataforma
// (Platform e PlatformLiveID são opcionais em CreateSessionRequest desde a
// campanha guarda-chuva) — quem exigia a mídia era só o formulário.
//
// Então aqui não há caminho novo: é a MESMA live.Service.Create que o painel
// chama, com a mídia deixada em branco. A mídia entra depois, pelo bloco 01 do
// simulador.
func (h *WebhookHandler) SimularEvento(c *fiber.Ctx) error {
	storeID, _ := c.Locals("store_id").(string)
	var req SimularEventoRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.DomainError(400, httpx.CodeValidationFailed, "corpo inválido")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	out, err := h.service.CreateSimulatedEvent(c.Context(), storeID, req.Titulo, req.DiasAteFechar)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	logger.From(c.Context(), h.logger).Info("live simulator created an event with a session",
		zap.String("store_id", storeID),
		zap.String("event_id", out.EventID),
		zap.String("session_id", out.SessionID),
	)
	return httpx.OK(c, out)
}

// SessaoSimulavel é uma sessão da loja onde dá para pendurar uma mídia.
type SessaoSimulavel struct {
	SessionID   string   `json:"sessionId"`
	Status      string   `json:"status"`
	EventID     string   `json:"eventId"`
	EventTitle  string   `json:"eventTitle"`
	StartedAt   string   `json:"startedAt,omitempty"`
	MidiasVivas []string `json:"midiasVivas"`
}

// SimularListarSessoes oferece as sessões recentes da loja.
//
// Existe para o painel não obrigar ninguém a colar um UUID na mão — e colar
// UUID errado é como se pendura mídia no evento errado.
func (h *WebhookHandler) SimularListarSessoes(c *fiber.Ctx) error {
	storeID, _ := c.Locals("store_id").(string)
	sessoes, err := h.service.ListSessionsForSimulator(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, sessoes)
}

// =============================================================================
// MÍDIA
// =============================================================================

// SimularMidiaRequest pede uma mídia de live falsa vinculada a uma sessão.
type SimularMidiaRequest struct {
	SessionID string `json:"sessionId"`
	// MediaID é opcional: em branco, o simulador inventa um id no formato do
	// Instagram. Preenchido, permite reencenar um caso real — colar o media id
	// que apareceu num log de produção e reproduzir o fluxo em cima dele.
	MediaID string `json:"mediaId"`
}

// Validate é o portão ozzo da convenção. Aqui ele não é burocracia: sessionId
// vindo vazio vincularia mídia a lugar nenhum, e o erro só apareceria como uma
// falha de UUID lá no repositório.
func (r SimularMidiaRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.SessionID, validation.Required, is.UUIDv4),
		validation.Field(&r.MediaID, validation.Length(0, 128)),
	)
}

// SimularMidiaResponse devolve o id que os comentários devem usar.
type SimularMidiaResponse struct {
	MediaID   string `json:"mediaId"`
	SessionID string `json:"sessionId"`
}

// SimularMidia vincula uma mídia inventada a uma sessão, como o Instagram faria
// quando a transmissão começa.
func (h *WebhookHandler) SimularMidia(c *fiber.Ctx) error {
	storeID, _ := c.Locals("store_id").(string)
	var req SimularMidiaRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.DomainError(400, httpx.CodeValidationFailed, "corpo inválido")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	mediaID := strings.TrimSpace(req.MediaID)
	if mediaID == "" {
		mediaID = novoMediaIDSimulado()
	}

	if err := h.service.AttachSimulatedMedia(c.Context(), storeID, req.SessionID, mediaID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	logger.From(c.Context(), h.logger).Info("live simulator attached a media to a session",
		zap.String("store_id", storeID),
		zap.String("session_id", req.SessionID),
		zap.String("media_id", mediaID),
	)
	return httpx.OK(c, SimularMidiaResponse{MediaID: mediaID, SessionID: req.SessionID})
}

// SimularEncerrarMidia solta a mídia da sessão — o equivalente a a transmissão
// acabar. Depois disso, comentário naquele media id não resolve mais a sessão,
// que é justamente o caso que se quer poder testar.
func (h *WebhookHandler) SimularEncerrarMidia(c *fiber.Ctx) error {
	storeID, _ := c.Locals("store_id").(string)
	mediaID := c.Params("mediaId")
	if mediaID == "" {
		return httpx.DomainError(422, httpx.CodeValidationFailed, "mediaId é obrigatório")
	}
	if err := h.service.ReleaseSimulatedMedia(c.Context(), storeID, mediaID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	logger.From(c.Context(), h.logger).Info("live simulator released a media",
		zap.String("store_id", storeID),
		zap.String("media_id", mediaID),
	)
	return httpx.OK(c, fiber.Map{"mediaId": mediaID, "released": true})
}

// =============================================================================
// COMENTÁRIO
// =============================================================================

// SimularComentarioRequest é um comentário como a compradora escreveria.
type SimularComentarioRequest struct {
	MediaID string `json:"mediaId"`
	// Handle é o @ da compradora, com ou sem arroba.
	Handle string `json:"handle"`
	// UserID é o id do Instagram dela. Em branco, o simulador inventa um
	// estável a partir do @ — assim o mesmo @ cai sempre no mesmo comprador
	// entre um teste e outro, que é o que permite testar carrinho acumulando.
	UserID string `json:"userId"`
	Text   string `json:"text"`
	// Vezes repete o mesmo comentário, para encenar rajada. 1 quando ausente.
	Vezes int `json:"vezes"`
}

// Validate: o teto de 25 vive aqui e não no handler porque é regra do pedido,
// não do caminho HTTP. Cada comentário vira escrita no ERP, e a conta de teste
// tem os mesmos 30/min da real — passar disso não testa mais nada, só enfileira.
func (r SimularComentarioRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.MediaID, validation.Required, validation.Length(1, 128)),
		validation.Field(&r.Handle, validation.Required, validation.Length(1, 64)),
		validation.Field(&r.Text, validation.Required, validation.Length(1, 500)),
		validation.Field(&r.Vezes, validation.Min(0), validation.Max(25)),
	)
}

// SimularComentarioResponse conta o que foi entregue.
type SimularComentarioResponse struct {
	Entregues []string `json:"entregues"`
	MediaID   string   `json:"mediaId"`
	Handle    string   `json:"handle"`
	UserID    string   `json:"userId"`
	Falhas    []string `json:"falhas,omitempty"`
}

// SimularComentario monta o payload que a Meta mandaria e o entrega ao mesmo
// processamento do webhook real.
func (h *WebhookHandler) SimularComentario(c *fiber.Ctx) error {
	var req SimularComentarioRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.DomainError(400, httpx.CodeValidationFailed, "corpo inválido")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	handle := normalizarArroba(req.Handle)
	if handle == "" {
		return httpx.DomainError(422, httpx.CodeValidationFailed, "handle é obrigatório")
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		userID = idEstavelDoHandle(handle)
	}
	vezes := req.Vezes
	if vezes < 1 {
		vezes = 1
	}

	resp := SimularComentarioResponse{MediaID: req.MediaID, Handle: handle, UserID: userID}
	contaIG, _ := c.Locals("store_id").(string)

	for i := 0; i < vezes; i++ {
		commentID := novoCommentIDSimulado()
		entry, change, corpo := montarComentarioIG(contaIG, req.MediaID, commentID, handle, userID, req.Text)

		// A MESMA função do webhook real. sigValid=true porque quem montou o
		// payload foi este servidor: não há assinatura a conferir, e fingir uma
		// inválida testaria a rejeição, não o fluxo.
		if err := h.processInstagramChange(c, entry, change, corpo, true); err != nil {
			resp.Falhas = append(resp.Falhas, fmt.Sprintf("%s: %v", commentID, err))
			continue
		}
		resp.Entregues = append(resp.Entregues, commentID)
	}

	logger.From(c.Context(), h.logger).Info("live simulator delivered comments",
		zap.String("media_id", req.MediaID),
		zap.String("handle", handle),
		zap.Int("entregues", len(resp.Entregues)),
		zap.Int("falhas", len(resp.Falhas)),
	)
	return httpx.OK(c, resp)
}

// montarComentarioIG constrói o payload EXATO que a Meta manda em live_comments.
//
// O corpo cru vai junto porque o processamento o guarda como evidência — é o
// que permite, depois, comparar um comentário simulado com um real lado a lado.
func montarComentarioIG(contaID, mediaID, commentID, handle, userID, texto string) (InstagramEntry, InstagramChange, []byte) {
	agora := time.Now()
	valor := InstagramLiveCommentValue{
		From:      InstagramUser{ID: userID, Username: handle},
		CommentID: commentID,
		Text:      texto,
		Media:     InstagramMedia{ID: mediaID, MediaProductType: "LIVE"},
	}
	change := InstagramChange{Field: "live_comments", Value: valor}
	entry := InstagramEntry{
		ID: contaID,
		// A Meta manda o carimbo da ENTREGA em MILISSEGUNDOS nos produtos de
		// Instagram. Mandar em segundos aqui esconderia o bug de conversão que
		// epochSeconds existe para tratar.
		Time:    agora.UnixMilli(),
		Changes: []InstagramChange{change},
	}
	corpo, _ := json.Marshal(InstagramWebhookPayload{
		Object: "instagram",
		Entry:  []InstagramEntry{entry},
	})
	return entry, change, corpo
}

// =============================================================================
// IDS
// =============================================================================

// novoMediaIDSimulado inventa um id no formato do Instagram, com prefixo que o
// denuncia. O prefixo não é enfeite: se um id destes aparecer num log de
// produção, é sinal de que alguma coisa vazou de staging para lá.
func novoMediaIDSimulado() string {
	return fmt.Sprintf("sim-media-%d", time.Now().UnixNano())
}

func novoCommentIDSimulado() string {
	return fmt.Sprintf("sim-comment-%d", time.Now().UnixNano())
}

// idEstavelDoHandle deriva um id de usuário do @, sempre o mesmo.
//
// Estável de propósito: o teste que importa é o carrinho ACUMULANDO entre
// comentários, e ele só acontece se o mesmo @ resolver o mesmo comprador. Um id
// aleatório a cada comentário criaria um comprador novo por vez e esconderia
// justamente o comportamento que se quer ver.
func idEstavelDoHandle(handle string) string {
	var soma uint64 = 14695981039346656037 // FNV-1a offset
	for _, r := range handle {
		soma ^= uint64(r)
		soma *= 1099511628211
	}
	return fmt.Sprintf("sim-user-%d", soma%1_000_000_000_000)
}

func normalizarArroba(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	return strings.ToLower(h)
}

// _ mantém o import de events referenciado — o simulador usa a mesma origem
// do webhook real quando o processamento pede a fonte.
var _ = events.SourceInstagramLive
