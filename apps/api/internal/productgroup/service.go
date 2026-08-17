package productgroup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	productpkg "livecart/apps/api/internal/product"
	productdomain "livecart/apps/api/internal/product/domain"
	"livecart/apps/api/internal/productgroup/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{repo: repo, logger: logger.Named("productgroup")}
}

// Create creates the group + options + values + variants atomically.
func (s *Service) Create(ctx context.Context, input CreateGroupInput) (*domain.CreateResult, error) {
	if err := validateCreateInput(input); err != nil {
		return nil, httpx.ErrUnprocessable(err.Error())
	}

	tx, err := s.repo.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlc.New(tx)

	group, err := domain.NewGroup(input.StoreID, input.Name, input.Description, input.ExternalID, input.ExternalSource)
	if err != nil {
		return nil, httpx.ErrUnprocessable(err.Error())
	}

	groupRow, err := q.CreateProductGroup(ctx, sqlc.CreateProductGroupParams{
		StoreID:        group.StoreID().ToPgUUID(),
		Name:           group.Name(),
		Description:    pgtype.Text{String: group.Description(), Valid: group.Description() != ""},
		ExternalID:     pgtype.Text{String: group.ExternalID(), Valid: group.ExternalID() != ""},
		ExternalSource: group.ExternalSource().String(),
	})
	if err != nil {
		return nil, fmt.Errorf("inserting group: %w", err)
	}
	groupID := groupRow.ID

	// optionValuesByOption[optionName][value] = optionValueUUID (pgtype)
	optionValuesByOption := make(map[string]map[string]pgtype.UUID, len(input.Options))
	for i, opt := range input.Options {
		optionRow, err := q.CreateProductOption(ctx, sqlc.CreateProductOptionParams{
			GroupID:  groupID,
			Name:     opt.Name,
			Position: int32(i),
		})
		if err != nil {
			return nil, fmt.Errorf("inserting option %q: %w", opt.Name, err)
		}
		valueMap := make(map[string]pgtype.UUID, len(opt.Values))
		for j, v := range opt.Values {
			vRow, err := q.CreateProductOptionValue(ctx, sqlc.CreateProductOptionValueParams{
				OptionID: optionRow.ID,
				Value:    v,
				Position: int32(j),
			})
			if err != nil {
				return nil, fmt.Errorf("inserting option value %q/%q: %w", opt.Name, v, err)
			}
			valueMap[v] = vRow.ID
		}
		optionValuesByOption[opt.Name] = valueMap
	}

	// Auto-generate keywords for variants that did not provide one.
	maxKw, err := q.GetMaxKeyword(ctx, input.StoreID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("getting max keyword: %w", err)
	}
	currentMax, _ := maxKw.(string)
	if currentMax == "" {
		currentMax = "0999"
	}

	createdVariants := make([]domain.CreatedVariant, 0, len(input.Variants))
	seenCombos := make(map[string]struct{}, len(input.Variants))

	for vIdx, v := range input.Variants {
		if len(v.OptionValues) != len(input.Options) {
			return nil, httpx.ErrUnprocessable(fmt.Sprintf("variant #%d: %s", vIdx+1, domain.ErrVariantOptionsMismatch.Error()))
		}

		// Resolve option_value IDs in the order of options (position-based mapping).
		valueIDs := make([]pgtype.UUID, len(v.OptionValues))
		comboKey := make([]string, len(v.OptionValues))
		for k, value := range v.OptionValues {
			optName := input.Options[k].Name
			id, ok := optionValuesByOption[optName][value]
			if !ok {
				return nil, httpx.ErrUnprocessable(fmt.Sprintf("variant #%d: option %q does not have value %q", vIdx+1, optName, value))
			}
			valueIDs[k] = id
			comboKey[k] = value
		}

		key := strings.Join(comboKey, "||")
		if _, dup := seenCombos[key]; dup {
			return nil, httpx.ErrUnprocessable(fmt.Sprintf("variant #%d: %s", vIdx+1, domain.ErrDuplicateVariant.Error()))
		}
		seenCombos[key] = struct{}{}

		keyword := v.Keyword
		if keyword == "" {
			next, err := productdomain.NextKeyword(currentMax)
			if err != nil {
				return nil, httpx.ErrUnprocessable("keyword range exhausted (max 9999)")
			}
			keyword = next.String()
			currentMax = keyword
		} else if !productdomain.IsValidKeyword(keyword) {
			return nil, httpx.ErrUnprocessable(fmt.Sprintf("variant #%d: invalid keyword %q", vIdx+1, keyword))
		}

		shipping, err := productpkg.ShippingDTOToDomain(v.Shipping)
		if err != nil {
			return nil, httpx.ErrUnprocessable(fmt.Sprintf("variant #%d: %s", vIdx+1, err.Error()))
		}

		variantName := buildVariantName(input.Name, v.OptionValues)

		productRow, err := q.CreateProduct(ctx, sqlc.CreateProductParams{
			StoreID:             input.StoreID.ToPgUUID(),
			Name:                variantName,
			ExternalID:          pgtype.Text{String: v.ExternalID, Valid: v.ExternalID != ""},
			ExternalSource:      input.ExternalSource.String(),
			Keyword:             keyword,
			Price:               pgtype.Int8{Int64: v.Price, Valid: true},
			ImageUrl:            pgtype.Text{String: v.ImageURL, Valid: v.ImageURL != ""},
			Stock:               pgtype.Int4{Int32: int32(v.Stock), Valid: true},
			WeightGrams:         intPtrToInt4(shipping.WeightGrams),
			HeightCm:            intPtrToInt4(shipping.HeightCm),
			WidthCm:             intPtrToInt4(shipping.WidthCm),
			LengthCm:            intPtrToInt4(shipping.LengthCm),
			Sku:                 pgtype.Text{String: shipping.SKU, Valid: shipping.SKU != ""},
			Barcode:             pgtype.Text{String: shipping.Barcode, Valid: shipping.Barcode != ""},
			PackageFormat:       packageFormat(shipping.PackageFormat),
			InsuranceValueCents: int64PtrToInt8(shipping.InsuranceValueCents),
			GroupID:             groupID,
		})
		if err != nil {
			return nil, fmt.Errorf("inserting variant #%d: %w", vIdx+1, err)
		}

		for _, optValueID := range valueIDs {
			if err := q.AssignVariantOption(ctx, sqlc.AssignVariantOptionParams{
				ProductID:     productRow.ID,
				OptionValueID: optValueID,
			}); err != nil {
				return nil, fmt.Errorf("assigning option to variant #%d: %w", vIdx+1, err)
			}
		}

		for imgIdx, url := range v.Images {
			if _, err := q.CreateProductImage(ctx, sqlc.CreateProductImageParams{
				ProductID: productRow.ID,
				Url:       url,
				Position:  int32(imgIdx),
			}); err != nil {
				return nil, fmt.Errorf("inserting variant image: %w", err)
			}
		}

		createdVariants = append(createdVariants, domain.CreatedVariant{
			ID:           pgUUIDToString(productRow.ID),
			Keyword:      keyword,
			OptionValues: append([]string(nil), v.OptionValues...),
		})
	}

	// Group images
	for i, url := range input.GroupImages {
		if _, err := q.CreateProductGroupImage(ctx, sqlc.CreateProductGroupImageParams{
			GroupID:  groupID,
			Url:      url,
			Position: int32(i),
		}); err != nil {
			return nil, fmt.Errorf("inserting group image: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return domain.NewCreateResult(
		pgUUIDToString(groupID),
		group.Name(),
		createdVariants,
		groupRow.CreatedAt.Time,
	), nil
}

// GetByID returns the full detail (options, values, variants, images) of a group
// as a domain aggregate. Presentation maps it via NewGroupDetailResponse.
func (s *Service) GetByID(ctx context.Context, id vo.ID, storeID vo.StoreID) (*domain.Detail, error) {
	group, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}

	options, err := s.repo.LoadOptions(ctx, group.ID())
	if err != nil {
		return nil, err
	}

	groupImages, err := s.repo.ListGroupImages(ctx, group.ID())
	if err != nil {
		return nil, err
	}

	variantRows, err := s.repo.ListVariantsByGroup(ctx, group.ID())
	if err != nil {
		return nil, fmt.Errorf("listing variants: %w", err)
	}

	variantOptions, err := s.repo.ListVariantOptionsByGroup(ctx, group.ID())
	if err != nil {
		return nil, fmt.Errorf("listing variant options: %w", err)
	}
	optsByVariant := make(map[string][]domain.OptionValuePair, len(variantRows))
	for _, opt := range variantOptions {
		key := pgUUIDToString(opt.ProductID)
		optsByVariant[key] = append(optsByVariant[key], domain.OptionValuePair{
			Option: opt.OptionName,
			Value:  opt.Value,
		})
	}

	variantImages, err := s.repo.ListVariantImagesByGroup(ctx, group.ID())
	if err != nil {
		return nil, fmt.Errorf("listing variant images: %w", err)
	}
	imgsByVariant := make(map[string][]domain.Image, len(variantRows))
	for _, img := range variantImages {
		key := pgUUIDToString(img.ProductID)
		imgsByVariant[key] = append(imgsByVariant[key], domain.Image{
			ID: pgUUIDToString(img.ID), URL: img.Url, Position: int(img.Position),
		})
	}

	variants := make([]domain.Variant, len(variantRows))
	for i, row := range variantRows {
		key := pgUUIDToString(row.ID)
		variants[i] = domain.Variant{
			ID:           key,
			Keyword:      row.Keyword,
			OptionValues: optsByVariant[key],
			Price:        row.Price.Int64,
			Stock:        int(row.Stock.Int32),
			SKU:          textOrEmpty(row.Sku),
			ImageURL:     textOrEmpty(row.ImageUrl),
			Images:       imgsByVariant[key],
		}
	}

	return domain.NewDetail(group, options, groupImages, variants), nil
}

func (s *Service) List(ctx context.Context, storeID vo.StoreID, limit, offset int) ([]domain.Summary, int, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.repo.ListByStore(ctx, storeID, limit, offset)
}

func (s *Service) Update(ctx context.Context, id vo.ID, storeID vo.StoreID, name, description string) (*domain.Detail, error) {
	group, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}
	if err := group.Update(name, description); err != nil {
		return nil, httpx.ErrUnprocessable(err.Error())
	}
	if err := s.repo.Update(ctx, group); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, id, storeID)
}

func (s *Service) Delete(ctx context.Context, id vo.ID, storeID vo.StoreID) error {
	// Existence check yields a 404 instead of silent no-op.
	if _, err := s.repo.GetByID(ctx, id, storeID); err != nil {
		return err
	}
	return s.repo.Delete(ctx, id, storeID)
}

func (s *Service) AddGroupImage(ctx context.Context, groupID vo.ID, storeID vo.StoreID, url string, position int) (domain.Image, error) {
	if _, err := s.repo.GetByID(ctx, groupID, storeID); err != nil {
		return domain.Image{}, err
	}
	return s.repo.AddGroupImage(ctx, groupID, url, position)
}

func (s *Service) DeleteGroupImage(ctx context.Context, imageID, groupID vo.ID, storeID vo.StoreID) error {
	if _, err := s.repo.GetByID(ctx, groupID, storeID); err != nil {
		return err
	}
	return s.repo.DeleteGroupImage(ctx, imageID, groupID)
}

// HasGroupForExternalID returns true when a product group already exists for the
// given (store, source, externalID) tuple. Used by ERP sync flows to decide
// between Create and per-variant update.
func (s *Service) HasGroupForExternalID(ctx context.Context, storeID vo.StoreID, source productdomain.ExternalSource, externalID string) (bool, error) {
	g, err := s.repo.GetByExternalID(ctx, storeID, source, externalID)
	if err != nil {
		return false, err
	}
	return g != nil, nil
}

// CreateForERP is a wrapper around Create used by ERP-import flows.
// It is identical to Create but accepts variants whose ExternalID is propagated
// to the created product rows so subsequent stock/price webhook updates can
// resolve the variant by external_id.
func (s *Service) CreateForERP(ctx context.Context, input CreateGroupInput) (*domain.CreateResult, error) {
	return s.Create(ctx, input)
}

// ============================================
// helpers
// ============================================

func validateCreateInput(in CreateGroupInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return domain.ErrGroupNameRequired
	}
	if len(in.Options) == 0 {
		return domain.ErrOptionsRequired
	}
	for _, o := range in.Options {
		if strings.TrimSpace(o.Name) == "" {
			return domain.ErrOptionNameRequired
		}
		if len(o.Values) == 0 {
			return domain.ErrOptionValuesRequired
		}
	}
	if len(in.Variants) == 0 {
		return domain.ErrVariantsRequired
	}
	return nil
}

func buildVariantName(groupName string, optionValues []string) string {
	if len(optionValues) == 0 {
		return groupName
	}
	return fmt.Sprintf("%s — %s", groupName, strings.Join(optionValues, " / "))
}

func packageFormat(f productdomain.PackageFormat) string {
	s := f.String()
	if s == "" {
		return string(productdomain.PackageFormatBox)
	}
	return s
}

func intPtrToInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func int64PtrToInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// ensure errors usage to avoid unused import
var _ = errors.New
