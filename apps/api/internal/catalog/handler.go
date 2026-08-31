package catalog

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes wires the authenticated, store-scoped catalog routes.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/catalogs")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
	g.Put("/:id/products", h.SetProducts)

	// Event association lives on live_events.catalog_id; managed here to keep all
	// catalog logic in one domain. Uses :id to match the live route param name.
	router.Get("/lives/:id/catalog", h.GetEventCatalog)
	router.Put("/lives/:id/catalog", h.SetEventCatalog)
}

// RegisterPublicRoutes wires the buyer-facing catalog route (no auth).
// Uses the /api/public prefix to bypass the auth middleware on /api/v1.
func (h *Handler) RegisterPublicRoutes(app fiber.Router) {
	app.Get("/api/public/events/:eventId/catalog", h.GetPublicCatalog)
}

func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateCatalogRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	productIDs, err := toProductIDs(req.ProductIDs)
	if err != nil {
		return err
	}
	cat, err := h.service.Create(c.UserContext(), storeID, req.Name, productIDs)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewCatalogResponse(cat))
}

func (h *Handler) List(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	items, err := h.service.List(c.UserContext(), storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListCatalogsResponse(items))
}

func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid catalog ID")
	}
	cat, products, err := h.service.GetByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewCatalogDetailResponse(cat, products))
}

func (h *Handler) Update(c *fiber.Ctx) error {
	var req UpdateCatalogRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid catalog ID")
	}
	cat, err := h.service.Update(c.UserContext(), id, storeID, req.Name)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewCatalogResponse(cat))
}

func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid catalog ID")
	}
	if err := h.service.Delete(c.UserContext(), id, storeID); err != nil {
		return err
	}
	return httpx.Deleted(c, id.String())
}

func (h *Handler) SetProducts(c *fiber.Ctx) error {
	var req SetProductsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid catalog ID")
	}
	productIDs, err := toProductIDs(req.ProductIDs)
	if err != nil {
		return err
	}
	products, err := h.service.SetProducts(c.UserContext(), id, storeID, productIDs)
	if err != nil {
		return err
	}
	return httpx.OK(c, newCatalogProductResponses(products))
}

func (h *Handler) GetEventCatalog(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	eventID, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid event ID")
	}
	cat, err := h.service.GetEventCatalog(c.UserContext(), eventID, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewCatalogResponse(cat))
}

func (h *Handler) SetEventCatalog(c *fiber.Ctx) error {
	var req SetEventCatalogRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	eventID, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid event ID")
	}
	var catalogID *vo.ID
	if req.CatalogID != nil && *req.CatalogID != "" {
		cid, err := vo.NewID(*req.CatalogID)
		if err != nil {
			return httpx.ErrUnprocessable("invalid catalog ID")
		}
		catalogID = &cid
	}
	if err := h.service.SetEventCatalog(c.UserContext(), eventID, storeID, catalogID); err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{"eventId": eventID.String(), "catalogId": req.CatalogID})
}

func (h *Handler) GetPublicCatalog(c *fiber.Ctx) error {
	eventID, err := vo.NewID(c.Params("eventId"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid event ID")
	}
	cat, products, err := h.service.GetPublicCatalogByEvent(c.UserContext(), eventID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewPublicCatalogResponse(cat, products))
}

func toProductIDs(raw []string) ([]vo.ProductID, error) {
	out := make([]vo.ProductID, 0, len(raw))
	for _, s := range raw {
		pid, err := vo.NewProductID(s)
		if err != nil {
			return nil, httpx.ErrUnprocessable("invalid product ID: " + s)
		}
		out = append(out, pid)
	}
	return out, nil
}
