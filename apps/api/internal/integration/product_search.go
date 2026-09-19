package integration

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"livecart/apps/api/db/sqlc"
	"net"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
	vo "livecart/apps/api/lib/valueobject"
)

// Include quota waits while remaining below the browser and server budgets.
// The UI uses 30s for listing and 50s for selected details; server writes use 60s.
const (
	erpSearchTimeout         = 25 * time.Second
	erpProductDetailsTimeout = 45 * time.Second
)

type SearchERPProductsRequest struct {
	Search      string `query:"search" json:"search"`
	Limit       int    `query:"limit" json:"limit"`
	SummaryOnly bool   `query:"summary" json:"summary"`
}

func (r SearchERPProductsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Search, validation.Required, validation.Length(2, 200)),
		validation.Field(&r.Limit, validation.Required, validation.Min(1), validation.Max(20)))
}

func (r SearchERPProductsRequest) ToInput(storeID, integrationID string) (SearchProductsInput, error) {
	search := strings.TrimSpace(r.Search)
	if len(search) < 2 {
		return SearchProductsInput{}, httpx.ErrUnprocessable("Digite pelo menos dois caracteres")
	}
	store, err := vo.NewID(storeID)
	if err != nil {
		return SearchProductsInput{}, httpx.ErrUnprocessable("Loja inválida")
	}
	integration, err := vo.NewID(integrationID)
	if err != nil {
		return SearchProductsInput{}, httpx.ErrUnprocessable("Integração inválida")
	}
	return SearchProductsInput{StoreID: store.String(), IntegrationID: integration.String(), Search: search, PageSize: r.Limit, SummaryOnly: r.SummaryOnly}, nil
}

func productSearchError(err error) error {
	if err == nil {
		return nil
	}
	var limited *ratelimit.ErrRateLimited
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &limited) ||
		(errors.As(err, &networkError) && networkError.Timeout()) {
		return httpx.DomainError(503, httpx.CodeErpThrottled, "O ERP está ocupado. Aguarde alguns segundos e tente novamente.")
	}
	return err
}

// GetERPProductDetails only reads the selected product. Available stock still
// comes from the ERP stock endpoint, never from a list's absent stock field.
func (s *Service) GetERPProductDetails(ctx context.Context, storeID, integrationID, productID string) (product *providers.ERPProduct, err error) {
	started := time.Now()
	defer func() {
		logger.From(ctx, s.logger).Info("ERP selected product lookup completed",
			zap.String("store_id", storeID), zap.String("integration_id", integrationID),
			zap.String("external_product_id", productID), zap.Duration("duration", time.Since(started)),
			zap.Bool("success", err == nil))
	}()
	if strings.TrimSpace(productID) == "" || strings.ContainsAny(productID, "/?#") {
		return nil, httpx.ErrUnprocessable("Produto inválido")
	}
	provider, err := s.GetERPProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}
	return readERPProductDetails(ctx, provider, productID)
}

type importProductReader interface {
	GetProductForImport(context.Context, string) (*providers.ERPProduct, error)
}

func getProductForImport(ctx context.Context, provider providers.ERPProvider, id string) (*providers.ERPProduct, error) {
	if reader, ok := provider.(importProductReader); ok {
		return reader.GetProductForImport(ctx, id)
	}
	return provider.GetProduct(ctx, id)
}

func readERPProductDetails(ctx context.Context, provider providers.ERPProvider, productID string) (*providers.ERPProduct, error) {
	product, err := getProductForImport(ctx, provider, productID)
	if err != nil {
		return nil, productSearchError(fmt.Errorf("reading selected ERP product: %w", err))
	}
	if product == nil {
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}
	if err := ctx.Err(); err != nil {
		return nil, productSearchError(err)
	}
	if !product.Active {
		return nil, httpx.DomainError(422, httpx.CodeErpProductInactive, "Produto inativo no ERP")
	}
	if product.IsParent && len(product.Variants) > 0 {
		return product, nil // Stock is confirmed only for the chosen variants at import.
	}
	if provider.Name() == providers.ProviderTiny && !tinyImportStockKnown(product) {
		return nil, erpImportStockUnavailable()
	}
	preview := newERPProductResponse(product)
	if !preview.Active || preview.Stock <= 0 {
		return nil, httpx.DomainError(422, httpx.CodeStockInsufficient, "Produto encontrado, mas sem estoque disponível no momento")
	}
	return product, nil
}

func erpImportStockUnavailable() error {
	return httpx.DomainError(503, httpx.CodeErpThrottled,
		"Produto encontrado, mas o ERP ainda não confirmou o estoque disponível. Aguarde e tente novamente.")
}

func tinyImportStockKnown(product *providers.ERPProduct) bool {
	if product == nil {
		return false
	}
	if product.IsParent && len(product.Variants) > 0 {
		for _, variant := range product.Variants {
			if variant.Active && !variant.StockKnown {
				return false
			}
		}
		return true
	}
	return product.StockKnown
}

func newERPProductResponse(product *providers.ERPProduct) ERPProductResponse {
	response := ERPProductResponse{ID: product.ID, SKU: product.SKU, GTIN: product.GTIN, Name: product.Name, Description: product.Description, Price: product.Price, Stock: product.Stock, Active: product.Active, ImageURL: product.ImageURL, ImageURLs: product.ImageURLs, IsParent: product.IsParent, Shipping: shippingPreviewFromERP(product.Shipping, product.WeightGramsHint)}
	if product.IsParent && len(product.Variants) > 0 {
		response.Stock = 0
		for _, v := range product.Variants {
			response.Stock += v.Stock
			response.Variants = append(response.Variants, ERPVariantResponse{StockKnown: v.StockKnown, ID: v.ID, SKU: v.SKU, GTIN: v.GTIN, Name: v.Name, Price: v.Price, Stock: v.Stock, Active: v.Active, ImageURL: v.ImageURL, Shipping: shippingPreviewFromERP(v.Shipping, v.WeightGramsHint), Attributes: v.Attributes})
		}
	}
	return response
}

// ReadCatalogProduct is the server-side authority for the simple creation form.
// The caller supplies only the store and external identity, never credentials.
func (s *Service) ReadCatalogProduct(ctx context.Context, storeID, source, id string) (*providers.ERPProduct, error) {
	row, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", source)
	if err != nil {
		return nil, err
	}
	provider, err := s.GetERPProvider(ctx, row.ID, storeID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "/?#") {
		return nil, httpx.ErrUnprocessable("Produto inválido")
	}
	p, err := getProductForImport(ctx, provider, id)
	if err != nil {
		return nil, productSearchError(err)
	}
	if p == nil {
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}
	if p.IsParent {
		return nil, httpx.ErrUnprocessable("Escolha as variantes para importar este produto")
	}
	if err := validateImportStock(ctx, provider, p); err != nil {
		return nil, err
	}
	return p, nil
}

func validateImportStock(ctx context.Context, provider providers.ERPProvider, p *providers.ERPProduct) error {
	if err := ctx.Err(); err != nil {
		return productSearchError(err)
	}
	if !p.Active {
		return httpx.DomainError(422, httpx.CodeErpProductInactive, "Produto inativo no ERP")
	}
	if provider.Name() == providers.ProviderTiny && !p.StockKnown {
		return erpImportStockUnavailable()
	}
	return nil // Known zero is a valid catalogue stock, never replaced by physical stock.
}

func (s *Service) markCatalogImportState(ctx context.Context, storeID, source string, products []ERPProductResponse) error {
	if s.repo == nil {
		return nil
	}
	for i := range products {
		p := &products[i]
		if !p.IsParent {
			continue
		}
		store, err := parseUUID(storeID)
		if err != nil {
			return err
		}
		_, err = s.repo.queries.GetProductGroupByExternalID(ctx, sqlc.GetProductGroupByExternalIDParams{
			StoreID: store, ExternalSource: source, ExternalID: pgtype.Text{String: p.ID, Valid: true},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		p.GroupImported = err == nil
		if s.productSyncer == nil || len(p.Variants) == 0 {
			continue
		}
		ids := make([]string, 0, len(p.Variants))
		for _, v := range p.Variants {
			ids = append(ids, v.ID)
		}
		saved, err := s.productSyncer.FilterRegisteredExternalIDs(ctx, storeID, source, ids)
		if err != nil {
			return err
		}
		registered := make(map[string]bool, len(saved))
		for _, id := range saved {
			registered[id] = true
		}
		for j := range p.Variants {
			p.Variants[j].AlreadyImported = registered[p.Variants[j].ID]
		}
	}
	return nil
}

func (r ImportERPProductRequest) Validate() error {
	return validation.ValidateStruct(&r, validation.Field(&r.VariantIDs,
		validation.Length(0, 1000), validation.Each(validation.Required, validation.Length(1, 200))))
}

func (r ImportERPProductRequest) ToInput(storeID, integrationID, productID string) (ImportERPProductInput, error) {
	store, err := vo.NewID(storeID)
	if err != nil {
		return ImportERPProductInput{}, httpx.ErrUnprocessable("Loja inválida")
	}
	integration, err := vo.NewID(integrationID)
	if err != nil {
		return ImportERPProductInput{}, httpx.ErrUnprocessable("Integração inválida")
	}
	if strings.TrimSpace(productID) == "" || strings.ContainsAny(productID, "/?#") {
		return ImportERPProductInput{}, httpx.ErrUnprocessable("Produto inválido")
	}
	return ImportERPProductInput{StoreID: store.String(), IntegrationID: integration.String(), TinyProductID: productID, VariantIDs: r.VariantIDs}, nil
}

// MarkERPImportState decorates a selected preview with store-scoped import state.
func (s *Service) MarkERPImportState(ctx context.Context, storeID, integrationID string, previews []ERPProductResponse) error {
	row, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return err
	}
	return s.markCatalogImportState(ctx, row.StoreID, row.Provider, previews)
}
