package integration

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// Superfície HTTP da publicação agendada (RN-31).
//
// Multipart e não JSON porque o asset chega junto: o caminho síncrono já é
// assim para Reel e Story, e um upload separado só para agendar criaria um
// arquivo órfão sempre que o lojista desistisse no meio.

// ScheduleInstagramPublishRequest é o pedido de agendamento. Os campos chegam
// como strings do multipart e são convertidos ANTES do Validate — o ozzo é o
// portão sintático sobre valores já tipados, não um parser.
type ScheduleInstagramPublishRequest struct {
	MediaKind              string     `json:"mediaKind"`
	ScheduledFor           *time.Time `json:"scheduledFor"`
	Caption                string     `json:"caption"`
	Title                  string     `json:"title"`
	ProductIDs             []string   `json:"productIds"`
	StartsAt               *time.Time `json:"startsAt"`
	EndsAt                 *time.Time `json:"endsAt"`
	CartExpirationMinutes  *int       `json:"cartExpirationMinutes"`
	CartMaxQuantityPerItem *int       `json:"cartMaxQuantityPerItem"`
}

// Validate é o portão sintático. As regras de negócio (lead mínimo, horizonte,
// coerência da janela com a data da publicação) ficam no service — são
// invariantes que dependem do relógio, não da forma do payload.
func (r ScheduleInstagramPublishRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.MediaKind, validation.Required, validation.In("post", "reel", "story")),
		validation.Field(&r.ScheduledFor, validation.Required),
		validation.Field(&r.ProductIDs, validation.Required, validation.Length(1, 0)),
		validation.Field(&r.Title, validation.Length(0, 200)),
		validation.Field(&r.Caption, validation.Length(0, 2200)),
		// Piso de 15 min pareado com Required: sem o Required o ozzo pula a
		// regra no zero-value e o piso vira mole (gotcha documentado na
		// convenção). Ponteiro nil continua significando "usar o da loja".
		validation.Field(&r.CartExpirationMinutes,
			validation.When(r.CartExpirationMinutes != nil,
				validation.Required, validation.Min(15), validation.Max(43200))),
		validation.Field(&r.CartMaxQuantityPerItem,
			validation.When(r.CartMaxQuantityPerItem != nil,
				validation.Required, validation.Min(1), validation.Max(999))),
	)
}

// ToInput traduz o Request no input do usecase, já com o asset resolvido.
func (r ScheduleInstagramPublishRequest) ToInput(storeID, assetPath, assetContentType string) SchedulePublishInput {
	return SchedulePublishInput{
		StoreID:                storeID,
		MediaKind:              r.MediaKind,
		AssetPath:              assetPath,
		AssetContentType:       assetContentType,
		Caption:                r.Caption,
		Title:                  r.Title,
		ProductIDs:             r.ProductIDs,
		StartsAt:               r.StartsAt,
		EndsAt:                 r.EndsAt,
		CartExpirationMinutes:  r.CartExpirationMinutes,
		CartMaxQuantityPerItem: r.CartMaxQuantityPerItem,
		ScheduledFor:           *r.ScheduledFor,
	}
}

// PublishJobResponse é o agendamento como o painel o vê.
type PublishJobResponse struct {
	ID                     string     `json:"id"`
	MediaKind              string     `json:"mediaKind"`
	Status                 string     `json:"status"`
	Title                  string     `json:"title"`
	Caption                string     `json:"caption"`
	ProductIDs             []string   `json:"productIds"`
	ScheduledFor           time.Time  `json:"scheduledFor"`
	StartsAt               *time.Time `json:"startsAt,omitempty"`
	EndsAt                 *time.Time `json:"endsAt,omitempty"`
	CartExpirationMinutes  *int       `json:"cartExpirationMinutes,omitempty"`
	CartMaxQuantityPerItem *int       `json:"cartMaxQuantityPerItem,omitempty"`
	EventID                string     `json:"eventId,omitempty"`
	SessionID              string     `json:"sessionId,omitempty"`
	PublishedMediaID       string     `json:"publishedMediaId,omitempty"`
	Attempts               int        `json:"attempts"`
	// LastError é exposto de propósito: um agendamento que falhou sem motivo
	// visível é a mesma armadilha da comunicação não entregue (RN-38) — o
	// lojista descobre pela venda que não aconteceu.
	LastError   string     `json:"lastError,omitempty"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	CancelledAt *time.Time `json:"cancelledAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// NewPublishJobResponse mapeia o job de domínio para a resposta.
func NewPublishJobResponse(job PublishJob) PublishJobResponse {
	return PublishJobResponse{
		ID:                     job.ID,
		MediaKind:              job.MediaKind,
		Status:                 job.Status,
		Title:                  job.Title,
		Caption:                job.Caption,
		ProductIDs:             job.ProductIDs,
		ScheduledFor:           job.ScheduledFor,
		StartsAt:               job.StartsAt,
		EndsAt:                 job.EndsAt,
		CartExpirationMinutes:  job.CartExpirationMinutes,
		CartMaxQuantityPerItem: job.CartMaxQuantityPerItem,
		EventID:                job.EventID,
		SessionID:              job.SessionID,
		PublishedMediaID:       job.PublishedMediaID,
		Attempts:               job.Attempts,
		LastError:              job.LastError,
		PublishedAt:            job.PublishedAt,
		CancelledAt:            job.CancelledAt,
		CreatedAt:              job.CreatedAt,
	}
}

// ScheduleInstagramPublish agenda a publicação de um post, reel ou story.
// @Summary Schedule an Instagram publication
// @Tags integrations
// @Accept multipart/form-data
// @Produce json
// @Param storeId path string true "Store ID"
// @Param file formData file true "Media (JPEG image or MP4 video)"
// @Success 201 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/scheduled-publications [post]
// @Security BearerAuth
func (h *Handler) ScheduleInstagramPublish(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)

	req, err := parseSchedulePublishForm(c)
	if err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.ErrBadRequest("file is required")
	}
	contentType := file.Header.Get("Content-Type")
	if err := validateInstagramAsset(req.MediaKind, contentType, file.Size); err != nil {
		return err
	}
	if h.s3Client == nil {
		return httpx.ErrUnprocessable("storage not configured")
	}

	src, openErr := file.Open()
	if openErr != nil {
		return httpx.ErrBadRequest("failed to read file")
	}
	defer src.Close()

	// O asset sobe AGORA e fica retido até a data. Nenhuma URL assinada é
	// gerada aqui: ela duraria horas e o agendamento dura dias.
	assetPath, upErr := h.s3Client.UploadFile(c.UserContext(), src, file.Filename, contentType, "instagram/"+storeID)
	if upErr != nil {
		return httpx.ErrUnprocessable("failed to upload the media")
	}

	job, err := h.service.SchedulePublish(c.UserContext(), req.ToInput(storeID, assetPath, contentType))
	if err != nil {
		// O agendamento não entrou: o arquivo recém-subido não tem mais dono.
		h.service.deleteTransientImage(c.UserContext(), assetPath)
		return err
	}
	return httpx.Created(c, NewPublishJobResponse(*job))
}

// ListInstagramScheduledPublications lista os agendamentos da loja.
// @Summary List scheduled Instagram publications
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/scheduled-publications [get]
// @Security BearerAuth
func (h *Handler) ListInstagramScheduledPublications(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)

	status := c.Query("status")
	switch status {
	case "", "scheduled", "publishing", "published", "failed", "cancelled":
	default:
		return httpx.ErrBadRequest("invalid status filter")
	}
	limit, _ := strconv.Atoi(c.Query("limit"))

	jobs, err := h.service.ListPublishJobs(c.UserContext(), storeID, status, limit)
	if err != nil {
		return err
	}
	out := make([]PublishJobResponse, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, NewPublishJobResponse(job))
	}
	return httpx.OK(c, out)
}

// CancelInstagramScheduledPublication cancela um agendamento ainda não disparado.
// @Summary Cancel a scheduled Instagram publication
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param jobId path string true "Job ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/scheduled-publications/{jobId} [delete]
// @Security BearerAuth
func (h *Handler) CancelInstagramScheduledPublication(c *fiber.Ctx) error {
	job, err := h.service.CancelPublish(c.UserContext(), c.Params("jobId"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	return httpx.OK(c, NewPublishJobResponse(*job))
}

// parseSchedulePublishForm converte os campos do multipart nos tipos do
// Request. Formato inválido é 400 aqui (o corpo não é interpretável); regra de
// forma é 422 no Validate.
func parseSchedulePublishForm(c *fiber.Ctx) (ScheduleInstagramPublishRequest, error) {
	var req ScheduleInstagramPublishRequest
	req.MediaKind = c.FormValue("mediaKind")
	req.Caption = c.FormValue("caption")
	req.Title = c.FormValue("title")

	if raw := c.FormValue("productIds"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &req.ProductIDs); err != nil {
			return req, httpx.ErrBadRequest("invalid productIds")
		}
	}

	for _, f := range []struct {
		name string
		dst  **time.Time
	}{
		{"scheduledFor", &req.ScheduledFor},
		{"startsAt", &req.StartsAt},
		{"endsAt", &req.EndsAt},
	} {
		raw := c.FormValue(f.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return req, httpx.ErrBadRequest(fmt.Sprintf("invalid %s format", f.name))
		}
		*f.dst = &t
	}

	for _, f := range []struct {
		name string
		dst  **int
	}{
		{"cartExpirationMinutes", &req.CartExpirationMinutes},
		{"cartMaxQuantityPerItem", &req.CartMaxQuantityPerItem},
	} {
		raw := c.FormValue(f.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return req, httpx.ErrBadRequest(fmt.Sprintf("invalid %s", f.name))
		}
		*f.dst = &n
	}

	return req, nil
}

// validateInstagramAsset centraliza os limites de mídia do Instagram, que
// estavam repetidos nos três handlers síncronos com números levemente
// diferentes por espécie. Um só lugar para mudar quando a Meta mudar.
func validateInstagramAsset(mediaKind, contentType string, size int64) error {
	isJPEG := contentType == "image/jpeg" || contentType == "image/jpg"
	isVideo := contentType == "video/mp4" || contentType == "video/quicktime"

	const mb = 1024 * 1024
	switch mediaKind {
	case "post":
		if !isJPEG {
			return httpx.ErrBadRequest("invalid file type — Instagram requires a JPEG image")
		}
		if size > 8*mb {
			return httpx.ErrBadRequest("file too large, maximum size is 8MB")
		}
	case "reel":
		if !isVideo {
			return httpx.ErrBadRequest("invalid file type — Instagram Reels require an MP4 video")
		}
		if size > 300*mb {
			return httpx.ErrBadRequest("video too large, maximum size is 300MB")
		}
	case "story":
		switch {
		case isJPEG && size > 8*mb:
			return httpx.ErrBadRequest("image too large, maximum size is 8MB")
		case isVideo && size > 100*mb:
			return httpx.ErrBadRequest("video too large, maximum size is 100MB")
		case !isJPEG && !isVideo:
			return httpx.ErrBadRequest("invalid file type — send a JPEG photo or an MP4 video")
		}
	default:
		return httpx.ErrBadRequest("mediaKind must be post, reel or story")
	}
	return nil
}

// instagramAssetIsVideo diz se o asset é vídeo, para o PublishStory.
func instagramAssetIsVideo(contentType string) bool {
	return strings.HasPrefix(contentType, "video/")
}
