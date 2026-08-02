package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// EventCloseScheduler arms an ETA task that finalizes a timed post/story event
// at its ends_at, so the window closes precisely instead of waiting on the
// SweepEndedTimedEvents sweep (kept as a safety net). Implemented over the asynq
// client in main.go — the live package must not import events at the service
// level, so this stays a local interface wired via SetEventCloseScheduler.
type EventCloseScheduler interface {
	ScheduleEventClose(ctx context.Context, eventID, storeID string, at time.Time) error
}

// Notifier is the minimal notification surface this package depends on.
// The concrete implementation lives in the integration package; we declare
// a local interface to avoid an import cycle.
type Notifier interface {
	NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) error
}

// NotifyEventCheckoutParams mirrors the integration package params struct
// (Go duck typing only matches methods, so we declare the input shape here).
type NotifyEventCheckoutParams struct {
	StoreID        string
	EventID        string
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	CommentID      string
	TotalItems     int
	TotalValue     int64
}

// CustomerUpserter creates or updates a customer record from cart-creation
// inputs and returns the customer UUID. Wired from the customer package via
// SetCustomerUpserter to keep this package free of customer internals.
type CustomerUpserter interface {
	UpsertForCart(ctx context.Context, storeID, platformUserID, platformHandle string) (customerID string, err error)
}

type Service struct {
	repo             *Repository
	logger           *zap.Logger
	notifier         Notifier
	customerUpserter CustomerUpserter
	closeScheduler   EventCloseScheduler
	ingestRepo       IngestRepository

	// Comment ingestion core collaborators (Bloco B4b). All five are wired via
	// setters AFTER the services are built (integration.Service is the concrete
	// impl and depends on live.Service), so every use is nil-guarded lazily.
	// stockReserver breaks the erp import cycle (neutral ReserveParams);
	// billingGate/webhookAuditor/socialReplier are satisfied by integration.Service
	// directly; notificationSvc is imported directly (notification has no cycle).
	stockReserver   StockReserver
	billingGate     BillingGate
	webhookAuditor  WebhookAuditor
	socialReplier   SocialReplier
	notificationSvc *notification.Service

	// core is the slice of this Service's own behaviour the comment consumer
	// reuses (session/event lookups, AddToCart, event-product whitelist). It
	// defaults to the Service itself (wired in NewService) so production is a
	// plain self-call; unit tests swap in a fake to exercise the core without a
	// database. See the commentCore interface in comment.go.
	core commentCore
}

// SetEventCloseScheduler wires the ETA scheduler for timed-event window close
// (optional — when unset, only SweepEndedTimedEvents finalizes timed events).
func (s *Service) SetEventCloseScheduler(sch EventCloseScheduler) { s.closeScheduler = sch }

func NewService(repo *Repository, logger *zap.Logger) *Service {
	s := &Service{
		repo:   repo,
		logger: logger.Named("live"),
	}
	// The comment core reuses this Service's own methods through the commentCore
	// seam; in production that is the Service itself.
	s.core = s
	return s
}

// SetStockReserver wires the local+ERP stock reservation collaborator used by the
// comment ingestion core. Optional — nil means stock reservations are skipped.
func (s *Service) SetStockReserver(r StockReserver) { s.stockReserver = r }

// SetBillingGate wires the paywall gate (PRD 007). Optional — nil means no gating.
func (s *Service) SetBillingGate(g BillingGate) { s.billingGate = g }

// SetWebhookAuditor wires the webhook-event audit sink. Optional — nil skips audit.
func (s *Service) SetWebhookAuditor(a WebhookAuditor) { s.webhookAuditor = a }

// SetSocialReplier wires the Instagram DM / comment-reply ACL. Optional — nil
// skips replies (max-quantity and post-commerce answers).
func (s *Service) SetSocialReplier(r SocialReplier) { s.socialReplier = r }

// SetNotificationService wires the notification service used for the immediate
// checkout DM/email. Optional — nil skips immediate notifications.
func (s *Service) SetNotificationService(svc *notification.Service) { s.notificationSvc = svc }

// SetNotifier wires a Notifier into the service after construction. This
// breaks the dependency cycle between live and integration packages
// (integration.Service depends on live.Service, and the notifier impl
// depends on integration.Service).
func (s *Service) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetCustomerUpserter wires a CustomerUpserter into the service. When set,
// AddToCart will materialize a customer row and link the cart to it before
// creating the cart, so the Customers tab and downstream analytics always
// reflect the latest activity.
func (s *Service) SetCustomerUpserter(u CustomerUpserter) {
	s.customerUpserter = u
}

// =============================================================================
// LEGACY API - Creates an event + session + platform in one call
// =============================================================================

// Create creates a live event with an optional initial session and platform.
// Uses transactions to ensure atomicity when session/platform are included.
func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	// Default to single type if not specified
	eventType := input.Type
	if eventType == "" {
		eventType = "single"
	}

	// Default close_cart_on_event_end to true if not specified
	closeCartOnEventEnd := true
	if input.CloseCartOnEventEnd != nil {
		closeCartOnEventEnd = *input.CloseCartOnEventEnd
	}

	// Determine initial status based on scheduling
	status := "active"
	if input.ScheduledAt != nil {
		status = "scheduled"
	}

	// If platform info is provided, create everything in a single transaction
	if input.Platform != nil && input.PlatformLiveID != nil && *input.Platform != "" && *input.PlatformLiveID != "" {
		event, session, _, err := s.repo.CreateEventWithSessionTx(ctx, CreateEventParams{
			StoreID:                input.StoreID,
			Title:                  input.Title,
			Type:                   eventType,
			Status:                 status,
			CloseCartOnEventEnd:    closeCartOnEventEnd,
			CartExpirationMinutes:  input.CartExpirationMinutes,
			CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
			SendOnLiveEnd:          input.SendOnLiveEnd,
			ScheduledAt:            input.ScheduledAt,
			Description:            input.Description,
		}, *input.Platform, *input.PlatformLiveID)
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to create live with session",
				zap.Error(err),
			)
			return CreateLiveOutput{}, err
		}

		logger.From(ctx, s.logger).Info("live created with session",
			zap.String("event_id", event.ID),
			zap.String("session_id", session.ID),
			zap.String("platform", *input.Platform),
			zap.String("platform_live_id", *input.PlatformLiveID),
		)

		// Persist the optional pix discount as a follow-up update — the column
		// is not yet wired into the sqlc-generated INSERT.
		if input.PixDiscountPercent != nil && *input.PixDiscountPercent > 0 {
			if err := s.repo.SetPixDiscountPercent(ctx, event.ID, input.StoreID, *input.PixDiscountPercent); err != nil {
				logger.From(ctx, s.logger).Warn("failed to set pix_discount_percent on create",
					zap.String("event_id", event.ID),
					zap.Error(err),
				)
			}
		}

		return CreateLiveOutput{
			ID:        event.ID,
			Title:     event.Title,
			Type:      event.Type,
			Platform:  *input.Platform,
			Status:    event.Status,
			CreatedAt: event.CreatedAt,
		}, nil
	}

	// No platform info - just create the event
	event, err := s.repo.CreateEvent(ctx, CreateEventParams{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		Type:                   eventType,
		Status:                 status,
		CloseCartOnEventEnd:    closeCartOnEventEnd,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
		SendOnLiveEnd:          input.SendOnLiveEnd,
		ScheduledAt:            input.ScheduledAt,
		Description:            input.Description,
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	if input.PixDiscountPercent != nil && *input.PixDiscountPercent > 0 {
		if err := s.repo.SetPixDiscountPercent(ctx, event.ID, input.StoreID, *input.PixDiscountPercent); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set pix_discount_percent on create",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	logger.From(ctx, s.logger).Info("live created without session",
		zap.String("event_id", event.ID),
		zap.String("type", eventType),
	)

	return CreateLiveOutput{
		ID:        event.ID,
		Title:     event.Title,
		Type:      event.Type,
		Platform:  "",
		Status:    event.Status,
		CreatedAt: event.CreatedAt,
	}, nil
}

// CreatePostEvent creates a post-commerce event mapped to a published Instagram
// post. It reuses the live create path (the post media id is stored as the
// session platform_live_id so comment processing finds the event), persists the
// post metadata, and whitelists the selected products for the promotion.
func (s *Service) CreatePostEvent(ctx context.Context, input CreatePostInput) (CreateLiveOutput, error) {
	if input.MediaID == "" {
		return CreateLiveOutput{}, httpx.ErrBadRequest("mediaId is required")
	}
	if len(input.ProductIDs) == 0 {
		return CreateLiveOutput{}, httpx.ErrBadRequest("select at least one product for the promotion")
	}

	platform := "instagram"
	closeCart := true
	eventType := input.Type
	if eventType == "" {
		eventType = "post"
	}
	// Create the event as 'active' regardless of a future start: the effective
	// status (and comment gating) is derived from the window, so the event is
	// always resolvable by media id. We do NOT pass ScheduledAt to Create here,
	// because that would set status='scheduled' and hide it from lookups.
	out, err := s.Create(ctx, CreateLiveInput{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		Type:                   eventType,
		Platform:               &platform,
		PlatformLiveID:         &input.MediaID,
		CloseCartOnEventEnd:    &closeCart,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	// Persist the optional start/end window (raw SQL columns).
	if input.StartsAt != nil || input.EndsAt != nil {
		if err := s.repo.SetEventWindow(ctx, out.ID, input.StoreID, input.StartsAt, input.EndsAt); err != nil {
			logger.From(ctx, s.logger).Warn("failed to set post event window",
				zap.String("event_id", out.ID), zap.Error(err))
		}
		// Arm the ETA close task at ends_at so the window finalizes on time.
		// Best-effort: SweepEndedTimedEvents is the backstop for a lost task.
		if input.EndsAt != nil && s.closeScheduler != nil {
			if err := s.closeScheduler.ScheduleEventClose(ctx, out.ID, input.StoreID, *input.EndsAt); err != nil {
				logger.From(ctx, s.logger).Warn("failed to schedule event window close",
					zap.String("event_id", out.ID), zap.Error(err))
			}
		}
	}

	// Persist post metadata (raw SQL columns, not in the sqlc INSERT).
	if err := s.repo.SetEventMedia(ctx, out.ID, input.StoreID, PostMediaInput{
		MediaID:      input.MediaID,
		Permalink:    input.MediaPermalink,
		ThumbnailURL: input.MediaThumbnailURL,
		Caption:      input.MediaCaption,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("failed to set post media metadata",
			zap.String("event_id", out.ID), zap.Error(err))
	}

	// Whitelist the selected products for this promotion.
	for i, productID := range input.ProductIDs {
		if _, err := s.AddEventProduct(ctx, AddEventProductInput{
			EventID:      out.ID,
			StoreID:      input.StoreID,
			ProductID:    productID,
			DisplayOrder: int32(i),
		}); err != nil {
			logger.From(ctx, s.logger).Warn("failed to whitelist product on post event",
				zap.String("event_id", out.ID),
				zap.String("product_id", productID),
				zap.Error(err))
		}
	}

	logger.From(ctx, s.logger).Info("post event created",
		zap.String("event_id", out.ID),
		zap.String("media_id", input.MediaID),
		zap.Int("product_count", len(input.ProductIDs)),
	)

	out.Platform = platform
	return out, nil
}

// MarkPostEventWebhookActive flags that a comments webhook arrived for the post
// event mapped to mediaID, so the polling capture stops handling it.
func (s *Service) MarkPostEventWebhookActive(ctx context.Context, mediaID string) error {
	return s.repo.MarkPostEventWebhookActive(ctx, mediaID)
}

// ListActivePostEvents returns active post events still served by polling.
func (s *Service) ListActivePostEvents(ctx context.Context) ([]PostEventRef, error) {
	return s.repo.ListActivePostEvents(ctx)
}

// EndPostEventByMediaID closes the post/story event mapped to mediaID when its
// media is gone on Instagram. D5: routes through End so the carts are finalized
// (moved to 'checkout' with expires_at armed → eligible for the expiry worker)
// and the ERP is reconciled — not just a bare status flip that left carts
// 'active' without a deadline forever. Falls back to the raw flip if the event
// can't be resolved.
func (s *Service) EndPostEventByMediaID(ctx context.Context, mediaID string) error {
	ref, err := s.repo.GetActiveTimedEventByMediaID(ctx, mediaID)
	if err != nil {
		return err
	}
	if ref == nil {
		// Nothing active to finalize; ensure polling stops anyway.
		return s.repo.EndPostEventByMediaID(ctx, mediaID)
	}
	// post.window_closed: the media was deleted / window closed (distinct from a
	// manual live end). Emitted before End (which fires event.ended).
	if err := s.repo.EmitPostWindowClosed(ctx, ref.EventID); err != nil {
		logger.From(ctx, s.logger).Error("failed to emit post.window_closed",
			zap.String("event_id", ref.EventID), zap.Error(err))
	}
	if _, err := s.End(ctx, EndLiveInput{ID: ref.EventID, StoreID: ref.StoreID}); err != nil {
		return err
	}
	return nil
}

// SweepEndedTimedEvents finalizes post/story events whose ends_at window has
// closed. D5: without it, a story (ends_at = now()+24h) or a scheduled-window
// post just changes its *effective* status on read while its carts stay
// 'active' without a deadline — the expiry worker never reaches them. End() is
// idempotent, so re-sweeping an already-ended event is a no-op.
func (s *Service) SweepEndedTimedEvents(ctx context.Context) {
	events, err := s.repo.ListEventsPastEndsAt(ctx, 200)
	if err != nil {
		logger.From(ctx, s.logger).Error("ends_at sweep: failed to list events", zap.Error(err))
		return
	}
	for _, ev := range events {
		evCtx := logger.WithStore(ctx, ev.StoreID, "")
		// post.window_closed before End (which fires event.ended). The list query
		// only returns still-active events, so this fires once per window close.
		if err := s.repo.EmitPostWindowClosed(evCtx, ev.EventID); err != nil {
			logger.From(evCtx, s.logger).Error("ends_at sweep: failed to emit post.window_closed",
				zap.String("event_id", ev.EventID), zap.Error(err))
		}
		if _, err := s.End(evCtx, EndLiveInput{ID: ev.EventID, StoreID: ev.StoreID}); err != nil {
			logger.From(evCtx, s.logger).Error("ends_at sweep: failed to finalize event",
				zap.String("event_id", ev.EventID),
				zap.Error(err))
		}
	}
}

// RunScheduledEventClose is the event.window_close ETA-task handler: the precise
// per-event counterpart of SweepEndedTimedEvents. Guard-first — if the window
// was pushed out it re-arms, if the event is already ended it is a no-op — then
// runs the same finalization (post.window_closed + End) as the sweep. End is
// idempotent, so at-least-once redelivery is safe.
func (s *Service) RunScheduledEventClose(ctx context.Context, eventID, storeID string) error {
	ev, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil || ev == nil {
		// Not found / unreadable — the sweep is the backstop; don't retry forever.
		return nil
	}
	if ev.EndsAt == nil || ev.Status == "ended" {
		return nil // window removed, or already finalized
	}
	if ev.EndsAt.After(time.Now().UTC()) {
		// Window extended after the task was armed — re-arm for the new time.
		if s.closeScheduler != nil {
			return s.closeScheduler.ScheduleEventClose(ctx, eventID, storeID, *ev.EndsAt)
		}
		return nil
	}
	// post.window_closed before End (which fires event.ended), mirroring the sweep.
	if err := s.repo.EmitPostWindowClosed(ctx, eventID); err != nil {
		logger.From(ctx, s.logger).Error("scheduled close: failed to emit post.window_closed",
			zap.String("event_id", eventID), zap.Error(err))
	}
	if _, err := s.End(ctx, EndLiveInput{ID: eventID, StoreID: storeID}); err != nil {
		return fmt.Errorf("scheduled close: finalizing event %s: %w", eventID, err)
	}
	return nil
}

// GetEventPulse returns the cheap change-signal used for near-real-time refresh.
func (s *Service) GetEventPulse(ctx context.Context, eventID, storeID string) (EventPulse, error) {
	return s.repo.GetEventPulse(ctx, eventID, storeID)
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (LiveOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Hydrate the pix-discount column (not yet wired through sqlc).
	if pct, err := s.repo.GetPixDiscountPercent(ctx, event.ID); err == nil {
		event.PixDiscountPercent = pct
	}

	// Get sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get platform info from first session
	var platform, platformLiveID string
	var startedAt, endedAt = event.CreatedAt, event.UpdatedAt
	var totalComments int

	if len(sessions) > 0 {
		firstSession := sessions[0]
		if firstSession.StartedAt != nil {
			startedAt = *firstSession.StartedAt
		}
		if firstSession.EndedAt != nil {
			endedAt = *firstSession.EndedAt
		}
		totalComments = firstSession.TotalComments

		platforms, err := s.repo.ListPlatformsBySession(ctx, firstSession.ID)
		if err == nil && len(platforms) > 0 {
			platform = platforms[0].Platform
			platformLiveID = platforms[0].PlatformLiveID
		}
	}

	return LiveOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Type:                   event.Type,
		Platform:               platform,
		PlatformLiveID:         platformLiveID,
		Status:                 EffectiveStatus(event.Status, event.ScheduledAt, event.EndsAt),
		StartedAt:              &startedAt,
		EndedAt:                &endedAt,
		TotalComments:          totalComments,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		PixDiscountPercent:     event.PixDiscountPercent,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
	}, nil
}

// GetEventWithSessions returns an event with all its sessions and platforms
func (s *Service) GetEventWithSessions(ctx context.Context, id, storeID string) (EventOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return EventOutput{}, err
	}

	if pct, err := s.repo.GetPixDiscountPercent(ctx, event.ID); err == nil {
		event.PixDiscountPercent = pct
	}

	// Get sessions for this event
	sessionRows, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return EventOutput{}, err
	}

	// Build session outputs with platforms and stats
	sessions := make([]SessionOutput, len(sessionRows))
	for i, sessionRow := range sessionRows {
		platforms, err := s.repo.ListPlatformsBySession(ctx, sessionRow.ID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to list platforms for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
			platforms = []PlatformRow{}
		}

		platformOutputs := make([]PlatformOutput, len(platforms))
		for j, p := range platforms {
			platformOutputs[j] = PlatformOutput{
				ID:             p.ID,
				SessionID:      p.SessionID,
				Platform:       p.Platform,
				PlatformLiveID: p.PlatformLiveID,
				AddedAt:        p.AddedAt,
			}
		}

		// Get session stats (carts and revenue)
		var totalCarts, paidCarts int
		var totalRevenue, paidRevenue int64
		stats, err := s.repo.GetSessionStats(ctx, sessionRow.ID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to get session stats",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
		} else {
			totalCarts = stats.TotalCarts
			paidCarts = stats.PaidCarts
			totalRevenue = stats.TotalRevenue
			paidRevenue = stats.PaidRevenue
		}

		// Get comments for this session (limit 100 for performance)
		var commentOutputs []CommentOutput
		commentRows, err := s.repo.ListCommentsBySession(ctx, sessionRow.ID, 100, 0)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to list comments for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
		} else {
			commentOutputs = make([]CommentOutput, len(commentRows))
			for k, c := range commentRows {
				commentOutputs[k] = CommentOutput{
					Handle: c.PlatformHandle,
					Text:   c.Text,
				}
			}
		}

		sessions[i] = SessionOutput{
			ID:            sessionRow.ID,
			EventID:       sessionRow.EventID,
			Status:        sessionRow.Status,
			StartedAt:     sessionRow.StartedAt,
			EndedAt:       sessionRow.EndedAt,
			TotalComments: sessionRow.TotalComments,
			TotalCarts:    totalCarts,
			PaidCarts:     paidCarts,
			TotalRevenue:  totalRevenue,
			PaidRevenue:   paidRevenue,
			Platforms:     platformOutputs,
			Comments:      commentOutputs,
			CreatedAt:     sessionRow.CreatedAt,
			UpdatedAt:     sessionRow.UpdatedAt,
		}
	}

	// Get product and upsell counts
	productCount, err := s.repo.CountEventProducts(ctx, event.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to count event products", zap.String("event_id", event.ID), zap.Error(err))
	}
	upsellCount, err := s.repo.CountEventUpsells(ctx, event.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to count event upsells", zap.String("event_id", event.ID), zap.Error(err))
	}

	return EventOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Type:                   event.Type,
		Status:                 event.Status,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		PixDiscountPercent:     event.PixDiscountPercent,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		Description:            event.Description,
		ProductCount:           productCount,
		UpsellCount:            upsellCount,
		Sessions:               sessions,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
	}, nil
}

func (s *Service) List(ctx context.Context, input ListLivesInput) (ListLivesOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	lives, total, err := s.repo.ListLives(ctx, ListLivesParams{
		StoreID: input.StoreID,
		Search:  input.Search,
		Pagination: struct {
			Limit  int
			Offset int
		}{
			Limit:  input.Pagination.Limit,
			Offset: input.Pagination.Offset(),
		},
		Sorting: struct {
			SortBy    string
			SortOrder string
		}{
			SortBy:    input.Sorting.SortBy,
			SortOrder: input.Sorting.SortOrder,
		},
		Filters: input.Filters,
	})
	if err != nil {
		return ListLivesOutput{}, err
	}

	return ListLivesOutput{
		Lives:      lives,
		Total:      total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateLiveInput) (LiveOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Update event title
	event, err := s.repo.UpdateEventTitle(ctx, input.ID, input.Title)
	if err != nil {
		return LiveOutput{}, err
	}

	// Optional pix-discount update — clamp range defensively even though the
	// handler/validator already gates it.
	if input.PixDiscountPercent != nil {
		pct := *input.PixDiscountPercent
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		if err := s.repo.SetPixDiscountPercent(ctx, event.ID, input.StoreID, pct); err != nil {
			return LiveOutput{}, err
		}
	}

	// Get full live output
	return s.GetByID(ctx, event.ID, input.StoreID)
}

func (s *Service) Start(ctx context.Context, id, storeID string) (LiveOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get active session for this event
	session, err := s.repo.GetActiveSessionByEvent(ctx, id)
	if err != nil {
		return LiveOutput{}, err
	}
	if session == nil {
		return LiveOutput{}, fmt.Errorf("no active session found for event")
	}

	// Start the session
	_, err = s.repo.StartSession(ctx, session.ID)
	if err != nil {
		return LiveOutput{}, err
	}

	return s.GetByID(ctx, id, storeID)
}

func (s *Service) End(ctx context.Context, input EndLiveInput) (EndLiveOutput, error) {
	// 0. Idempotency guard: if the event was already ended, do not re-run
	// the finalization side-effects (carts, DMs). Return the current state.
	existing, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}
	if existing != nil && existing.Status == "ended" {
		liveOutput, _ := s.GetByID(ctx, existing.ID, input.StoreID)
		return EndLiveOutput{
			Live:           liveOutput,
			CartsFinalized: 0,
			AutoSendLinks:  false,
		}, nil
	}

	// 1. End the event
	event, err := s.repo.EndEvent(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}

	// 2. End all active sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, input.ID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list sessions for event",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	} else {
		for _, session := range sessions {
			if session.Status == "active" || session.Status == "live" {
				_, err := s.repo.EndSession(ctx, session.ID)
				if err != nil {
					logger.From(ctx, s.logger).Error("failed to end session",
						zap.String("session_id", session.ID),
						zap.Error(err),
					)
				}
			}
		}
	}

	// 3. Finalize all pending carts (now tied to event)
	cartsFinalized, err := s.repo.FinalizeCartsByEvent(ctx, input.ID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to finalize carts",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	}

	// Capture the slug before spawning detached goroutines: the request ctx is
	// recycled by fasthttp when the response is sent.
	storeSlug, _ := ctx.Value(logger.StoreSlugKey).(string)

	// 4. Determine if we should auto-send checkout links.
	// Carts are unique per (event_id, platform_user_id), so multi-session
	// events already aggregate items per buyer. Use the explicit override
	// when provided, otherwise fall back to the store default.
	sessionCount, _ := s.repo.CountSessionsByEvent(ctx, input.ID)
	autoSend := false
	if input.AutoSend != nil {
		autoSend = *input.AutoSend
	} else {
		storeDefault, err := s.repo.GetStoreAutoSendSetting(ctx, input.StoreID)
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to get store auto_send setting",
				zap.Error(err),
			)
		} else {
			autoSend = storeDefault
		}
	}

	logger.From(ctx, s.logger).Info("live event ended",
		zap.String("event_id", input.ID),
		zap.Int("session_count", sessionCount),
		zap.Int("carts_finalized", cartsFinalized),
		zap.Bool("auto_send_links", autoSend),
	)

	// 5. Dispatch checkout DMs (best-effort, async — never blocks the response).
	if autoSend && s.notifier != nil {
		go s.sendCheckoutLinksForEvent(logger.WithStore(context.Background(), input.StoreID, storeSlug), input.StoreID, event.ID)
	}

	// Get full output
	liveOutput, _ := s.GetByID(ctx, event.ID, input.StoreID)

	return EndLiveOutput{
		Live:           liveOutput,
		CartsFinalized: cartsFinalized,
		AutoSendLinks:  autoSend,
	}, nil
}

// sendCheckoutLinksForEvent iterates over all carts of an event with at least
// one item and dispatches a checkout DM per buyer through the configured
// Notifier. Errors are logged individually and never interrupt the loop.
func (s *Service) sendCheckoutLinksForEvent(ctx context.Context, storeID, eventID string) {
	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list carts for event checkout dispatch",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return
	}

	sent := 0
	skipped := 0
	for _, c := range carts {
		if c.TotalItems <= 0 || c.PlatformUserID == "" {
			skipped++
			continue
		}
		// Only notify carts that were just finalized for checkout. Skip
		// carts already paid, expired, or abandoned during the live.
		if c.Status != "checkout" {
			skipped++
			continue
		}
		if c.PaymentStatus != nil && *c.PaymentStatus == "paid" {
			skipped++
			continue
		}
		commentID, _ := s.repo.GetLatestCommentIDByUser(ctx, eventID, c.PlatformUserID)
		if err := s.notifier.NotifyEventCheckout(ctx, NotifyEventCheckoutParams{
			StoreID:        storeID,
			EventID:        eventID,
			CartID:         c.ID,
			CartToken:      c.Token,
			PlatformUserID: c.PlatformUserID,
			PlatformHandle: c.PlatformHandle,
			CommentID:      commentID,
			TotalItems:     c.TotalItems,
			TotalValue:     c.TotalValue,
		}); err != nil {
			logger.From(ctx, s.logger).Warn("failed to notify event checkout",
				zap.String("event_id", eventID),
				zap.String("cart_id", c.ID),
				zap.String("platform_user_id", c.PlatformUserID),
				zap.Error(err),
			)
			continue
		}
		sent++
	}

	logger.From(ctx, s.logger).Info("event checkout dispatch finished",
		zap.String("event_id", eventID),
		zap.Int("sent", sent),
		zap.Int("skipped", skipped),
		zap.Int("total", len(carts)),
	)
}

func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.DeleteEvent(ctx, id, storeID)
}

func (s *Service) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

// =============================================================================
// SESSION OPERATIONS
// =============================================================================

// CreateSession creates a new session within an event.
// Uses a transaction to ensure atomicity of session + platform creation.
func (s *Service) CreateSession(ctx context.Context, input CreateSessionInput) (CreateSessionOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return CreateSessionOutput{}, err
	}

	// Create the session and add platform in a single transaction
	session, platform, err := s.repo.CreateSessionWithPlatformTx(ctx, input.EventID, input.Platform, input.PlatformLiveID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create session with platform",
			zap.String("event_id", input.EventID),
			zap.Error(err),
		)
		return CreateSessionOutput{}, err
	}

	logger.From(ctx, s.logger).Info("session created",
		zap.String("event_id", input.EventID),
		zap.String("session_id", session.ID),
		zap.String("platform", input.Platform),
	)

	return CreateSessionOutput{
		ID:      session.ID,
		EventID: session.EventID,
		Status:  session.Status,
		Platform: PlatformOutput{
			ID:             platform.ID,
			SessionID:      platform.SessionID,
			Platform:       platform.Platform,
			PlatformLiveID: platform.PlatformLiveID,
			AddedAt:        platform.AddedAt,
		},
		CreatedAt: session.CreatedAt,
	}, nil
}

// GetSessionByPlatformLiveID finds an active session by platform live ID.
func (s *Service) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionOutput, error) {
	session, err := s.repo.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	// Get platforms for this session
	platforms, err := s.repo.ListPlatformsBySession(ctx, session.ID)
	if err != nil {
		return nil, err
	}

	platformOutputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		platformOutputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return &SessionOutput{
		ID:            session.ID,
		EventID:       session.EventID,
		Status:        session.Status,
		StartedAt:     session.StartedAt,
		EndedAt:       session.EndedAt,
		TotalComments: session.TotalComments,
		Platforms:     platformOutputs,
		CreatedAt:     session.CreatedAt,
		UpdatedAt:     session.UpdatedAt,
	}, nil
}

// GetEventByPlatformLiveID finds an active event by any associated platform live ID.
func (s *Service) GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventOutput, error) {
	event, err := s.repo.GetEventByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, nil
	}

	return &EventOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Type:                   event.Type,
		Status:                 event.Status,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		CurrentActiveProductID: event.CurrentActiveProductID,
		ProcessingPaused:       event.ProcessingPaused,
		ScheduledAt:            event.ScheduledAt,
		EndsAt:                 event.EndsAt,
		Description:            event.Description,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
	}, nil
}

// =============================================================================
// PLATFORM OPERATIONS
// =============================================================================

// AddPlatform adds a platform ID to a session.
func (s *Service) AddPlatform(ctx context.Context, input AddPlatformInput) (PlatformOutput, error) {
	row, err := s.repo.AddPlatformToSession(ctx, input.SessionID, input.Platform, input.PlatformLiveID)
	if err != nil {
		return PlatformOutput{}, err
	}

	logger.From(ctx, s.logger).Info("platform added to session",
		zap.String("session_id", input.SessionID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return PlatformOutput{
		ID:             row.ID,
		SessionID:      row.SessionID,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt,
	}, nil
}

// ListPlatforms returns all platforms for a session.
func (s *Service) ListPlatforms(ctx context.Context, sessionID string) ([]PlatformOutput, error) {
	platforms, err := s.repo.ListPlatformsBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	outputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		outputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return outputs, nil
}

// RemovePlatform removes a platform from a session.
func (s *Service) RemovePlatform(ctx context.Context, sessionID, platformLiveID string) error {
	return s.repo.RemovePlatformFromSession(ctx, sessionID, platformLiveID)
}

// =============================================================================
// CART OPERATIONS
// =============================================================================

// AddToCart adds a product to a user's cart during a live event.
// Creates a new cart if one doesn't exist for this user in this event.
func (s *Service) AddToCart(ctx context.Context, input AddToCartInput) (AddToCartOutput, error) {
	// Generate token for new carts
	token, err := generateCartToken()
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("generating cart token: %w", err)
	}

	// Resolve customer for this cart so the Customers tab reflects activity
	// in real time. Best-effort: a failure here must not block adding to cart
	// (the cart still works, the customer link is just missing).
	customerID := input.CustomerID
	if customerID == nil && s.customerUpserter != nil && input.StoreID != "" && input.PlatformUserID != "" {
		id, upsertErr := s.customerUpserter.UpsertForCart(ctx, input.StoreID, input.PlatformUserID, input.PlatformHandle)
		if upsertErr != nil {
			logger.From(ctx, s.logger).Warn("failed to upsert customer for cart",
				zap.String("store_id", input.StoreID),
				zap.String("platform_user_id", input.PlatformUserID),
				zap.Error(upsertErr),
			)
		} else if id != "" {
			customerID = &id
		}
	}

	// Get or create cart for this user in this event. Thread the session so a
	// new cart records its originating session.
	var originSession *string
	if input.SessionID != "" {
		originSession = &input.SessionID
	}
	cart, isNew, err := s.repo.GetOrCreateCart(ctx, GetOrCreateCartParams{
		EventID:        input.EventID,
		SessionID:      originSession,
		PlatformUserID: input.PlatformUserID,
		PlatformHandle: input.PlatformHandle,
		Token:          token,
		CustomerID:     customerID,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("getting or creating cart: %w", err)
	}

	// Add item to cart, attributed to the session it was added in (first-touch).
	err = s.repo.AddCartItem(ctx, AddCartItemParams{
		CartID:             cart.ID,
		ProductID:          input.ProductID,
		SessionID:          input.SessionID,
		Quantity:           input.Quantity,
		UnitPrice:          input.ProductPrice,
		WaitlistedQuantity: input.WaitlistedQuantity,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("adding item to cart: %w", err)
	}

	// Get updated cart totals
	totalItems, totalCents, err := s.repo.GetCartTotals(ctx, cart.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get cart totals", zap.Error(err))
		// Continue with zero totals - notification can still be sent
	}

	logger.From(ctx, s.logger).Info("added product to cart",
		zap.String("cart_id", cart.ID),
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
		zap.Int("quantity", input.Quantity),
		zap.Bool("new_cart", isNew),
		zap.Int("total_items", totalItems),
		zap.Int64("total_cents", totalCents),
	)

	return AddToCartOutput{
		CartID:     cart.ID,
		CartToken:  cart.Token,
		IsNewCart:  isNew,
		TotalItems: totalItems,
		TotalCents: totalCents,
	}, nil
}

// generateCartToken creates a random token for cart checkout URLs.
func generateCartToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

// GetEventStats returns stats for an event (comments, carts, revenue).
func (s *Service) GetEventStats(ctx context.Context, eventID, storeID string) (EventStatsOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	stats, err := s.repo.GetEventStats(ctx, eventID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	return EventStatsOutput{
		TotalComments:     stats.TotalComments,
		TotalCarts:        stats.TotalCarts,
		OpenCarts:         stats.OpenCarts,
		CheckoutCarts:     stats.CheckoutCarts,
		PaidCarts:         stats.PaidCarts,
		TotalProductsSold: stats.TotalProductsSold,
		ProjectedRevenue:  stats.ProjectedRevenue,
		ConfirmedRevenue:  stats.ConfirmedRevenue,
	}, nil
}

// ListCartsWithTotalByEvent returns all carts for an event with total value and item count.
func (s *Service) ListCartsWithTotalByEvent(ctx context.Context, eventID, storeID string) ([]CartWithTotalOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]CartWithTotalOutput, len(carts))
	for i, cart := range carts {
		outputs[i] = CartWithTotalOutput{
			ID:              cart.ID,
			Token:           cart.Token,
			SessionID:       cart.SessionID,
			PlatformUserID:  cart.PlatformUserID,
			PlatformHandle:  cart.PlatformHandle,
			Status:          cart.Status,
			PaymentStatus:   cart.PaymentStatus,
			TotalValue:      cart.TotalValue,
			TotalItems:      cart.TotalItems,
			AvailableItems:  cart.AvailableItems,
			WaitlistedItems: cart.WaitlistedItems,
			CreatedAt:       cart.CreatedAt,
			ExpiresAt:       cart.ExpiresAt,
		}
	}

	return outputs, nil
}

// ResendCheckoutMessage re-sends the Instagram checkout DM for a single cart.
// It lets the merchant manually re-deliver the checkout link to a buyer from the
// event detail UI (for example, when the buyer missed the original message).
func (s *Service) ResendCheckoutMessage(ctx context.Context, eventID, cartID, storeID string) (CartWithTotalOutput, error) {
	if s.notifier == nil {
		return CartWithTotalOutput{}, httpx.ErrUnprocessable("instagram notifications are not configured")
	}

	// Verify event exists and belongs to store (authorization).
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return CartWithTotalOutput{}, err
	}

	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		return CartWithTotalOutput{}, err
	}

	var cart *CartWithTotalOutput
	for i := range carts {
		if carts[i].ID == cartID {
			c := CartWithTotalOutput{
				ID:              carts[i].ID,
				Token:           carts[i].Token,
				SessionID:       carts[i].SessionID,
				PlatformUserID:  carts[i].PlatformUserID,
				PlatformHandle:  carts[i].PlatformHandle,
				Status:          carts[i].Status,
				PaymentStatus:   carts[i].PaymentStatus,
				TotalValue:      carts[i].TotalValue,
				TotalItems:      carts[i].TotalItems,
				AvailableItems:  carts[i].AvailableItems,
				WaitlistedItems: carts[i].WaitlistedItems,
				CreatedAt:       carts[i].CreatedAt,
				ExpiresAt:       carts[i].ExpiresAt,
			}
			cart = &c
			break
		}
	}
	if cart == nil {
		return CartWithTotalOutput{}, httpx.ErrNotFound("cart not found")
	}
	if cart.PlatformUserID == "" {
		return CartWithTotalOutput{}, httpx.ErrUnprocessable("cart has no Instagram recipient")
	}
	if cart.TotalItems <= 0 {
		return CartWithTotalOutput{}, httpx.ErrUnprocessable("cart has no items to send")
	}

	// Prefer delivering via a private reply to the buyer's last comment (7-day
	// window) rather than a direct message by IGSID (24h window opened only by an
	// inbound DM). A comment does not open the DM window, so without this the
	// resend is rejected with error 2534022 even moments after the comment.
	commentID, lookupErr := s.repo.GetLatestCommentIDByUser(ctx, eventID, cart.PlatformUserID)
	if lookupErr != nil {
		// Don't abort — the DM path may still work — but make the failure visible
		// instead of silently degrading to DM-only (which fails outside the 24h
		// window with error 2534022).
		logger.From(ctx, s.logger).Error("failed to look up buyer's latest comment for private reply",
			zap.String("event_id", eventID),
			zap.String("platform_user_id", cart.PlatformUserID),
			zap.Error(lookupErr),
		)
	}
	logger.From(ctx, s.logger).Info("resend checkout message: delivery context",
		zap.String("event_id", eventID),
		zap.String("cart_id", cartID),
		zap.String("platform_user_id", cart.PlatformUserID),
		zap.String("comment_id", commentID),
	)

	if err := s.notifier.NotifyEventCheckout(ctx, NotifyEventCheckoutParams{
		StoreID:        storeID,
		EventID:        eventID,
		CartID:         cart.ID,
		CartToken:      cart.Token,
		PlatformUserID: cart.PlatformUserID,
		PlatformHandle: cart.PlatformHandle,
		CommentID:      commentID,
		TotalItems:     cart.TotalItems,
		TotalValue:     cart.TotalValue,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("failed to resend checkout message",
			zap.String("event_id", eventID),
			zap.String("cart_id", cartID),
			zap.String("platform_user_id", cart.PlatformUserID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		// Outside-the-window rejection (IG error 2534022): tell the merchant the
		// concrete fix instead of a generic failure.
		if strings.Contains(err.Error(), "2534022") {
			return CartWithTotalOutput{}, httpx.ErrUnprocessable(
				"O Instagram só permite enviar a mensagem se o comprador comentou recentemente ou mandou uma DM para a loja. Peça para o comprador comentar de novo na live (ou enviar uma DM) e clique em reenviar em seguida.")
		}
		return CartWithTotalOutput{}, httpx.ErrUnprocessable("failed to send Instagram message")
	}

	logger.From(ctx, s.logger).Info("checkout message resent",
		zap.String("event_id", eventID),
		zap.String("cart_id", cartID),
		zap.String("platform_user_id", cart.PlatformUserID),
	)
	return *cart, nil
}

// ListCommentsByEvent returns the event's comments (with Instagram comment IDs)
// for the moderation UI. Validates store ownership of the event.
func (s *Service) ListCommentsByEvent(ctx context.Context, eventID, storeID string, limit, offset int) ([]CommentRow, error) {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return s.repo.ListCommentsByEvent(ctx, eventID, limit, offset)
}

// ListActiveCheckouts returns the carts in checkout phase for an event so the
// merchant can watch buyer activity in real time before payment lands.
func (s *Service) ListActiveCheckouts(ctx context.Context, eventID, storeID string) ([]ActiveCheckoutOutput, error) {
	if _, err := s.repo.GetEventByID(ctx, eventID, storeID); err != nil {
		return nil, err
	}
	return s.repo.ListActiveCheckoutsByEvent(ctx, eventID)
}

// ListProductsByEvent returns all products sold in an event with quantity and revenue.
func (s *Service) ListProductsByEvent(ctx context.Context, eventID, storeID string) ([]EventProductSalesOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	products, err := s.repo.ListProductsByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]EventProductSalesOutput, len(products))
	for i, product := range products {
		outputs[i] = EventProductSalesOutput{
			ID:            product.ID,
			Name:          product.Name,
			ImageURL:      product.ImageURL,
			Keyword:       product.Keyword,
			TotalQuantity: product.TotalQuantity,
			TotalRevenue:  product.TotalRevenue,
		}
	}

	return outputs, nil
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// SetActiveProduct sets or clears the active product for an event
func (s *Service) SetActiveProduct(ctx context.Context, eventID, storeID string, productID *string) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.ErrBadRequest("can only set active product on active events")
	}

	// Set or clear active product
	if productID != nil && *productID != "" {
		_, err = s.repo.SetActiveProduct(ctx, eventID, storeID, *productID)
	} else {
		_, err = s.repo.ClearActiveProduct(ctx, eventID, storeID)
	}
	if err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("active product updated",
		zap.String("event_id", eventID),
		zap.Stringp("product_id", productID),
	)

	// Return updated state
	return s.GetLiveModeState(ctx, eventID, storeID)
}

// SetProcessingPaused pauses or resumes comment processing for an event
func (s *Service) SetProcessingPaused(ctx context.Context, eventID, storeID string, paused bool) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.ErrBadRequest("can only change processing state on active events")
	}

	_, err = s.repo.SetProcessingPaused(ctx, eventID, storeID, paused)
	if err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("processing paused state updated",
		zap.String("event_id", eventID),
		zap.Bool("paused", paused),
	)

	// Return updated state
	return s.GetLiveModeState(ctx, eventID, storeID)
}

// GetLiveModeState returns the current live mode state for an event
func (s *Service) GetLiveModeState(ctx context.Context, eventID, storeID string) (*LiveModeStateOutput, error) {
	return s.repo.GetLiveModeState(ctx, eventID, storeID)
}

// =============================================================================
// EVENT PRODUCTS (Whitelist)
// =============================================================================

// AddEventProduct adds a product to an event's whitelist
func (s *Service) AddEventProduct(ctx context.Context, input AddEventProductInput) (EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventProductOutput{}, err
	}

	output, err := s.repo.AddEventProduct(ctx, input)
	if err != nil {
		return EventProductOutput{}, err
	}

	logger.From(ctx, s.logger).Info("added product to event whitelist",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
	)

	return output, nil
}

// ListEventProducts returns all products in an event's whitelist
func (s *Service) ListEventProducts(ctx context.Context, eventID, storeID string) ([]EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	return s.repo.ListEventProducts(ctx, eventID)
}

// UpdateEventProduct updates a product's configuration in an event
func (s *Service) UpdateEventProduct(ctx context.Context, input UpdateEventProductInput) (EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventProductOutput{}, err
	}

	output, err := s.repo.UpdateEventProduct(ctx, input)
	if err != nil {
		return EventProductOutput{}, err
	}

	logger.From(ctx, s.logger).Info("updated event product",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ID),
	)

	return output, nil
}

// DeleteEventProduct removes a product from an event's whitelist
func (s *Service) DeleteEventProduct(ctx context.Context, id, eventID, storeID string) error {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return err
	}

	if err := s.repo.DeleteEventProduct(ctx, id, eventID); err != nil {
		return err
	}

	logger.From(ctx, s.logger).Info("deleted event product",
		zap.String("event_id", eventID),
		zap.String("product_id", id),
	)

	return nil
}

// ValidateProductForEvent checks if a product can be sold in an event
func (s *Service) ValidateProductForEvent(ctx context.Context, eventID, productID, storeID string) (*ProductValidationResult, error) {
	return s.repo.GetEventProductConfig(ctx, eventID, productID, storeID)
}

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// AddEventUpsell adds an upsell to an event
func (s *Service) AddEventUpsell(ctx context.Context, input AddEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.AddEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	logger.From(ctx, s.logger).Info("added upsell to event",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
	)

	return output, nil
}

// ListEventUpsells returns all upsells for an event
func (s *Service) ListEventUpsells(ctx context.Context, eventID, storeID string) ([]EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	return s.repo.ListEventUpsells(ctx, eventID)
}

// UpdateEventUpsell updates an upsell's configuration
func (s *Service) UpdateEventUpsell(ctx context.Context, input UpdateEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.UpdateEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	logger.From(ctx, s.logger).Info("updated event upsell",
		zap.String("event_id", input.EventID),
		zap.String("upsell_id", input.ID),
	)

	return output, nil
}

// DeleteEventUpsell removes an upsell from an event
func (s *Service) DeleteEventUpsell(ctx context.Context, id, eventID, storeID string) error {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return err
	}

	if err := s.repo.DeleteEventUpsell(ctx, id, eventID); err != nil {
		return err
	}

	logger.From(ctx, s.logger).Info("deleted event upsell",
		zap.String("event_id", eventID),
		zap.String("upsell_id", id),
	)

	return nil
}
