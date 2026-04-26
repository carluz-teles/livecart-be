package productgroup

import (
	"context"
	"fmt"

	productpkg "livecart/apps/api/internal/product"
	productdomain "livecart/apps/api/internal/product/domain"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

// SyncerAdapter implements integration.ProductGroupSyncer so the integration
// service can hand off ERP products that carry variations (Tiny tipo=V, etc.)
// to this package without circular imports.
type SyncerAdapter struct {
	groupSvc   *Service
	productSvc *productpkg.Service
}

func NewSyncerAdapter(groupSvc *Service, productSvc *productpkg.Service) *SyncerAdapter {
	return &SyncerAdapter{groupSvc: groupSvc, productSvc: productSvc}
}

// SyncFromERP creates the group + variants on first sync, or updates each
// variant by external_id on subsequent syncs. The parent ERPProduct must carry
// IsParent=true and a populated Variants slice.
func (a *SyncerAdapter) SyncFromERP(ctx context.Context, storeIDStr, externalSourceStr string, parent providers.ERPProduct) error {
	if !parent.IsParent || len(parent.Variants) == 0 {
		return fmt.Errorf("SyncFromERP: parent product %q has no variants", parent.ID)
	}

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return fmt.Errorf("invalid store id: %w", err)
	}
	source, err := productdomain.NewExternalSource(externalSourceStr)
	if err != nil {
		return fmt.Errorf("invalid external source %q: %w", externalSourceStr, err)
	}

	exists, err := a.groupSvc.HasGroupForExternalID(ctx, storeID, source, parent.ID)
	if err != nil {
		return err
	}

	if !exists {
		options := buildOptionsFromVariants(parent.GradeKeys, parent.Variants)
		variants := buildVariantsFromERP(parent.Variants, options)
		var groupImages []string
		if parent.ImageURL != "" {
			groupImages = []string{parent.ImageURL}
		}
		_, err := a.groupSvc.CreateForERP(ctx, CreateGroupInput{
			StoreID:        storeID,
			Name:           parent.Name,
			Description:    parent.Description,
			ExternalID:     parent.ID,
			ExternalSource: source,
			Options:        options,
			GroupImages:    groupImages,
			Variants:       variants,
		})
		return err
	}

	// Group already exists: update each variant by external_id (price/stock/active).
	// New variants added on the ERP side are NOT yet auto-imported here — that requires
	// extending the option_value catalog and inserting new product rows; out of scope for v1.
	for _, v := range parent.Variants {
		ok, err := a.productSvc.HasProductByExternalID(ctx, storeID, source, v.ID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		money, _ := vo.NewMoney(v.Price)
		if _, err := a.productSvc.SyncFromERP(ctx, productpkg.SyncFromERPInput{
			StoreID:        storeID,
			ExternalID:     v.ID,
			ExternalSource: source,
			Name:           parent.Name + " — " + joinAttributes(v.Attributes),
			Price:          money,
			ImageURL:       v.ImageURL,
			Stock:          v.Stock,
			Active:         v.Active,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ImportFromERP creates a brand-new product_group in LiveCart from an ERP
// parent product whose `Variants` slice has already been filtered to the
// caller's desired subset. Unlike SyncFromERP it errors out if the group
// already exists (caller decides what to do — typically ask the user).
//
// Returns the new group UUID and the external IDs of the variants that were
// persisted, in input order.
func (a *SyncerAdapter) ImportFromERP(ctx context.Context, storeIDStr, externalSourceStr string, parent providers.ERPProduct) (string, []string, error) {
	if !parent.IsParent || len(parent.Variants) == 0 {
		return "", nil, fmt.Errorf("ImportFromERP: parent product %q has no variants", parent.ID)
	}

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid store id: %w", err)
	}
	source, err := productdomain.NewExternalSource(externalSourceStr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid external source %q: %w", externalSourceStr, err)
	}

	exists, err := a.groupSvc.HasGroupForExternalID(ctx, storeID, source, parent.ID)
	if err != nil {
		return "", nil, err
	}
	if exists {
		return "", nil, httpx.ErrConflict("grupo de produto já importado neste catálogo")
	}

	options := buildOptionsFromVariants(parent.GradeKeys, parent.Variants)
	variants := buildVariantsFromERP(parent.Variants, options)
	var groupImages []string
	if parent.ImageURL != "" {
		groupImages = []string{parent.ImageURL}
	}

	out, err := a.groupSvc.CreateForERP(ctx, CreateGroupInput{
		StoreID:        storeID,
		Name:           parent.Name,
		Description:    parent.Description,
		ExternalID:     parent.ID,
		ExternalSource: source,
		Options:        options,
		GroupImages:    groupImages,
		Variants:       variants,
	})
	if err != nil {
		return "", nil, err
	}

	// Variants are persisted in input order; map back to ERP external IDs.
	importedExternalIDs := make([]string, 0, len(variants))
	for i := range out.Variants {
		if i >= len(variants) {
			break
		}
		importedExternalIDs = append(importedExternalIDs, variants[i].ExternalID)
	}
	return out.ID, importedExternalIDs, nil
}

// buildOptionsFromVariants infers the option/value matrix from the variants when
// the parent didn't provide explicit grade keys (some Tiny payloads omit them on
// the parent and only carry them on each child).
func buildOptionsFromVariants(gradeKeys []string, variants []providers.ERPProduct) []OptionRequest {
	keys := append([]string(nil), gradeKeys...)
	if len(keys) == 0 {
		seen := map[string]struct{}{}
		for _, v := range variants {
			for k := range v.Attributes {
				if _, ok := seen[k]; !ok {
					seen[k] = struct{}{}
					keys = append(keys, k)
				}
			}
		}
	}
	out := make([]OptionRequest, len(keys))
	valuesSeen := make([]map[string]struct{}, len(keys))
	for i := range keys {
		valuesSeen[i] = map[string]struct{}{}
		out[i] = OptionRequest{Name: keys[i]}
	}
	for _, v := range variants {
		for i, k := range keys {
			val := v.Attributes[k]
			if val == "" {
				continue
			}
			if _, ok := valuesSeen[i][val]; ok {
				continue
			}
			valuesSeen[i][val] = struct{}{}
			out[i].Values = append(out[i].Values, val)
		}
	}
	return out
}

func buildVariantsFromERP(variants []providers.ERPProduct, options []OptionRequest) []VariantRequest {
	out := make([]VariantRequest, 0, len(variants))
	for _, v := range variants {
		ovs := make([]string, len(options))
		for i, opt := range options {
			ovs[i] = v.Attributes[opt.Name]
		}
		out = append(out, VariantRequest{
			OptionValues: ovs,
			Price:        v.Price,
			Stock:        v.Stock,
			SKU:          v.SKU,
			ImageURL:     v.ImageURL,
			ExternalID:   v.ID,
			Shipping:     erpShippingToDTO(v.Shipping),
		})
	}
	return out
}

// erpShippingToDTO maps the provider-side shipping profile to the HTTP DTO the
// productgroup service expects. Returns the zero value when the ERP did not
// supply one — the variant just won't be marked as shippable until the merchant
// edits it.
func erpShippingToDTO(s *providers.ERPShippingProfile) productpkg.ShippingProfileDTO {
	if s == nil {
		return productpkg.ShippingProfileDTO{}
	}
	w, h, wd, l := s.WeightGrams, s.HeightCm, s.WidthCm, s.LengthCm
	return productpkg.ShippingProfileDTO{
		WeightGrams:   &w,
		HeightCm:      &h,
		WidthCm:       &wd,
		LengthCm:      &l,
		PackageFormat: s.PackageFormat,
	}
}

func joinAttributes(attrs map[string]string) string {
	parts := make([]string, 0, len(attrs))
	for k, v := range attrs {
		parts = append(parts, fmt.Sprintf("%s: %s", k, v))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ensure errors usage
var _ = httpx.IsNotFound
