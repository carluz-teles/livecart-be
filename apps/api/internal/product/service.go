package product

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("product"),
	}
}

func (s *Service) Create(ctx context.Context, input CreateProductInput) (*domain.Product, error) {
	// Resolve keyword: validate if provided, or auto-generate
	keyword, err := s.resolveKeyword(ctx, input.StoreID, input.Keyword)
	if err != nil {
		return nil, err
	}

	// Check uniqueness
	existing, err := s.repo.GetByKeyword(ctx, input.StoreID, keyword)
	if err != nil && !httpx.IsNotFound(err) {
		return nil, fmt.Errorf("checking keyword uniqueness: %w", err)
	}
	if existing != nil {
		return nil, httpx.ErrConflict("keyword already in use")
	}

	// Create product via domain factory
	product, err := domain.NewProduct(
		input.StoreID,
		input.Name,
		input.ExternalID,
		input.ExternalSource,
		keyword,
		input.Price,
		input.ImageURL,
		input.Stock,
		input.Shipping,
	)
	if err != nil {
		return nil, fmt.Errorf("creating product: %w", err)
	}

	if input.GroupID != nil {
		product.AttachGroup(input.GroupID)
	}

	// Save to repository
	if err := s.repo.Save(ctx, product); err != nil {
		return nil, err
	}

	for i, url := range input.Images {
		if _, err := s.repo.AddImage(ctx, product.ID(), url, i); err != nil {
			return nil, fmt.Errorf("attaching variant image: %w", err)
		}
	}

	return product, nil
}

// resolveKeyword validates or auto-generates a keyword for a product.
func (s *Service) resolveKeyword(ctx context.Context, storeID vo.StoreID, inputKeyword string) (domain.Keyword, error) {
	if inputKeyword != "" {
		kw, err := domain.NewKeyword(inputKeyword)
		if err != nil {
			return domain.Keyword{}, httpx.ErrUnprocessable(fmt.Sprintf("invalid keyword: %s", err.Error()))
		}
		return kw, nil
	}

	// Auto-generate keyword
	maxKw, err := s.repo.GetMaxKeyword(ctx, storeID)
	if err != nil {
		return domain.Keyword{}, fmt.Errorf("getting max keyword: %w", err)
	}

	kw, err := domain.NextKeyword(maxKw)
	if err != nil {
		return domain.Keyword{}, httpx.ErrUnprocessable("keyword range exhausted (max 9999)")
	}

	return kw, nil
}

func (s *Service) GetByID(ctx context.Context, id vo.ProductID, storeID vo.StoreID) (*ProductView, error) {
	product, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}
	view := &ProductView{Product: product}

	images, err := s.repo.ListImages(ctx, id)
	if err != nil {
		return nil, err
	}
	view.Images = images

	if product.GroupID() != nil {
		opts, err := s.repo.ListVariantOptions(ctx, id)
		if err != nil {
			return nil, err
		}
		refs := make([]OptionValueRef, len(opts))
		for i, o := range opts {
			refs[i] = OptionValueRef{Option: o.OptionName, Value: o.Value}
		}
		view.OptionValues = refs
		// Group base name for the short title (best-effort).
		if info, err := s.repo.VariantInfoForProducts(ctx, []string{product.ID().String()}); err == nil {
			view.GroupName = info[product.ID().String()].GroupName
		}
	}
	return view, nil
}

func (s *Service) List(ctx context.Context, input ListProductsInput) ([]*ProductView, int, error) {
	// Normalize pagination and sorting
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListProductsParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return nil, 0, err
	}

	views := make([]*ProductView, len(result.Products))
	variantIDs := make([]string, 0)
	for i, product := range result.Products {
		views[i] = &ProductView{Product: product}
		if product.GroupID() != nil {
			variantIDs = append(variantIDs, product.ID().String())
		}
	}

	// Enrich variants with their group name + option values so the picker can
	// show "Camiseta · Cor: Preto · Tam: M" instead of one giant product name.
	if len(variantIDs) > 0 {
		if info, err := s.repo.VariantInfoForProducts(ctx, variantIDs); err != nil {
			logger.From(ctx, s.logger).Warn("failed to load variant info for product list", zap.Error(err))
		} else {
			for _, v := range views {
				if vi, ok := info[v.Product.ID().String()]; ok {
					v.GroupName = vi.GroupName
					v.OptionValues = vi.Options
				}
			}
		}
	}

	return views, result.Total, nil
}

func (s *Service) Update(ctx context.Context, input UpdateProductInput) (*ProductView, error) {
	// Get existing product
	product, err := s.repo.GetByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return nil, err
	}

	// Use domain method to update
	if err := product.UpdateDetails(input.Name, input.Price, input.ImageURL, input.Stock, input.Active, input.Shipping); err != nil {
		return nil, httpx.ErrUnprocessable(err.Error())
	}

	// Save changes
	if err := s.repo.Update(ctx, product); err != nil {
		return nil, err
	}

	return &ProductView{Product: product}, nil
}

func (s *Service) Delete(ctx context.Context, id vo.ProductID, storeID vo.StoreID) error {
	return s.repo.Delete(ctx, id, storeID)
}

// AddImage attaches one image URL to a variant gallery (after asserting ownership).
func (s *Service) AddImage(ctx context.Context, id vo.ProductID, storeID vo.StoreID, url string, position int) (string, error) {
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return "", err
	}
	return s.repo.AddImage(ctx, id, url, position)
}

// DeleteImage removes one image from a variant gallery.
func (s *Service) DeleteImage(ctx context.Context, id vo.ProductID, storeID vo.StoreID, imageID vo.ID) error {
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return err
	}
	return s.repo.DeleteImage(ctx, id, imageID)
}

// HasProductByExternalID checks if a product with the given external ID exists.
func (s *Service) HasProductByExternalID(ctx context.Context, storeID vo.StoreID, externalSource domain.ExternalSource, externalID string) (bool, error) {
	existing, err := s.repo.GetByExternalID(ctx, storeID, externalSource, externalID)
	if err != nil {
		return false, err
	}
	return existing != nil, nil
}

// FilterRegisteredExternalIDs returns the subset of externalIDs already
// imported for the store+source — used to flag "já cadastrado" in the ERP
// product search.
func (s *Service) FilterRegisteredExternalIDs(ctx context.Context, storeID vo.StoreID, externalSource domain.ExternalSource, externalIDs []string) ([]string, error) {
	return s.repo.ListRegisteredExternalIDs(ctx, storeID, externalSource, externalIDs)
}

// resolveSyncedStock decide o estoque local após um sync vindo do ERP:
//   - skipStock: preserva o local (fail-safe de erro de DB);
//   - downgradeOnly: janela do guard — aplica o valor do ERP só quando é MENOR
//     que o local (redução legítima do lojista, direção segura); um valor >=
//     local é eco de reserva ou inflação de finalização e é preservado, para
//     nunca inflar o local e disparar promoção fantasma da waitlist;
//   - caso normal: aplica o valor do ERP.
func resolveSyncedStock(localStock, erpStock int, skipStock, downgradeOnly bool) int {
	if skipStock {
		return localStock
	}
	// Saldo NEGATIVO no ERP não é estoque, é sintoma.
	//
	// O Tiny aceita o saldo ir abaixo de zero — está gravado na bateria de
	// sandbox: lançar sobre saldo 0 levou a -1, sem erro. Um número negativo lá
	// significa que uma saída passou do que existia, e nunca que o lojista tem
	// menos que nada. Copiá-lo para o nosso contador é copiar o defeito.
	//
	// Em 08/08 o Gabinete Gamer levou uma reserva de 3 sobre um saldo que já
	// estava em 0. O Tiny foi para -3 e ecoou -3 no webhook; o `downgrade_only`
	// aceitou por ser "menor que o local" e a escrita bateu na constraint
	// `stock cannot be negative`. O estrago não parou no estoque: o erro derruba
	// a sincronização INTEIRA do produto — nome, preço, dimensões — que
	// retentou quatro vezes e desistiu. O produto parou de sincronizar por causa
	// de um número que nunca deveria ter sido considerado.
	//
	// Preservar o local é o lado seguro: o saldo do ERP volta ao positivo assim
	// que o estorno correspondente entra, e o sync seguinte reconcilia sozinho.
	if erpStock < 0 {
		return localStock
	}
	if downgradeOnly && erpStock >= localStock {
		return localStock
	}
	return erpStock
}

// SyncFromERP updates an existing product from ERP data.
// Returns (true, nil) if updated, (false, nil) if product not found in LiveCart.
func (s *Service) SyncFromERP(ctx context.Context, input SyncFromERPInput) (bool, error) {
	existing, err := s.repo.GetByExternalID(ctx, input.StoreID, input.ExternalSource, input.ExternalID)
	if err != nil {
		return false, fmt.Errorf("looking up product by external ID: %w", err)
	}

	if existing == nil {
		return false, nil
	}

	localStock := existing.Stock()
	stock := resolveSyncedStock(localStock, input.Stock, input.SkipStock, input.DowngradeOnly)

	// O que o sync fez com o estoque, sempre — e alto quando ele REDUZ.
	//
	// Nenhum dos dois valores era registrado. Quando o webhook do Tiny gravou
	// 4 por cima de um 5 correto, em staging, não sobrou linha nenhuma dizendo
	// isso: o log só imprimia as FLAGS, não os números nem se algo mudou.
	// Reconstruir uma unidade perdida virou arqueologia sobre movimento de ERP.
	//
	// Reduzir é a direção que custa dinheiro: some estoque que o lojista tem.
	// Ela pode ser legítima (o lojista baixou no ERP), então é Warn e não erro
	// — mas nunca mais silêncio.
	if stock != localStock {
		lg := logger.From(ctx, s.logger)
		fields := []zap.Field{
			zap.String("external_id", input.ExternalID),
			zap.Int("local_stock", localStock),
			zap.Int("erp_stock", input.Stock),
			zap.Int("new_stock", stock),
			zap.Bool("skip_stock", input.SkipStock),
			zap.Bool("downgrade_only", input.DowngradeOnly),
		}
		if stock < localStock {
			lg.Warn("ERP sync REDUCED local stock", fields...)
		} else {
			lg.Info("ERP sync changed local stock", fields...)
		}
	}

	shipping := existing.Shipping()
	if input.Shipping != nil {
		shipping = *input.Shipping
	}

	if err := existing.UpdateDetails(input.Name, input.Price, input.ImageURL, stock, input.Active, shipping); err != nil {
		return false, fmt.Errorf("updating product details: %w", err)
	}
	if err := s.repo.Update(ctx, existing); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) GetStats(ctx context.Context, storeID vo.StoreID) (ProductStats, error) {
	return s.repo.GetStats(ctx, storeID)
}
