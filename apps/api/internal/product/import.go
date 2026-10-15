package product

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

const MaxImportRows = 500

// Pointers distinguish an omitted column from an explicit zero/false. Empty
// spreadsheet cells are omitted by the client and never erase existing fields.
type ImportProductRow struct {
	Row     int     `json:"row"`
	Keyword string  `json:"keyword,omitempty"`
	SKU     *string `json:"sku,omitempty"`
	Name    *string `json:"name,omitempty"`
	Price   *int64  `json:"price,omitempty"`
	Stock   *int    `json:"stock,omitempty"`
	Barcode *string `json:"barcode,omitempty"`
	Active  *bool   `json:"active,omitempty"`
}

type ImportProductsRequest struct {
	MatchBy  string             `json:"matchBy"`
	Rows     []ImportProductRow `json:"rows"`
	Revision string             `json:"revision,omitempty"`
}

type ImportProductsInput struct {
	StoreID  vo.StoreID
	MatchBy  string
	Rows     []ImportProductRow
	Revision string
}

func (r ImportProductsRequest) ToInput(storeID string) (ImportProductsInput, error) {
	sid, err := vo.NewStoreID(storeID)
	if err != nil {
		return ImportProductsInput{}, httpx.DomainError(422, httpx.CodeValidationFailed, "invalid store ID")
	}
	return ImportProductsInput{StoreID: sid, MatchBy: r.MatchBy, Rows: r.Rows, Revision: r.Revision}, nil
}

func (r ImportProductsRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.MatchBy, validation.Required, validation.In("keyword", "sku")),
		validation.Field(&r.Rows, validation.Required, validation.Length(1, MaxImportRows)),
		validation.Field(&r.Revision, validation.Length(0, 64)),
	)
}

type ImportProductResult struct {
	Row      int      `json:"row"`
	Action   string   `json:"action"`
	Keyword  string   `json:"keyword"`
	Name     string   `json:"name"`
	Price    int64    `json:"price"`
	Stock    int      `json:"stock"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}
type ImportProductsResponse struct {
	Rows     []ImportProductResult `json:"rows"`
	Created  int                   `json:"created"`
	Updated  int                   `json:"updated"`
	Invalid  int                   `json:"invalid"`
	Revision string                `json:"revision"`
	Applied  bool                  `json:"applied"`
}

type importPlan struct {
	response ImportProductsResponse
	products []*domain.Product
	existing []bool
}

// PreviewImports godoc
// @Summary Preview a product spreadsheet import without saving
// @Tags products
// @Accept json
// @Produce json
// @Param storeId path string true "Store UUID"
// @Param request body ImportProductsRequest true "Mapped rows, at most 500"
// @Success 200 {object} httpx.Envelope{data=ImportProductsResponse}
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/products/import/preview [post]
// @Security BearerAuth
func (h *Handler) PreviewImports(c *fiber.Ctx) error { return h.importProducts(c, false) }

// ApplyImports godoc
// @Summary Atomically apply a previously reviewed product import
// @Tags products
// @Accept json
// @Produce json
// @Param storeId path string true "Store UUID"
// @Param request body ImportProductsRequest true "Rows and revision returned by preview"
// @Success 200 {object} httpx.Envelope{data=ImportProductsResponse}
// @Failure 409 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/products/import [post]
// @Security BearerAuth
func (h *Handler) ApplyImports(c *fiber.Ctx) error { return h.importProducts(c, true) }

func (h *Handler) importProducts(c *fiber.Ctx, apply bool) error {
	var req ImportProductsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	result, err := h.service.ImportProducts(c.UserContext(), input, apply)
	if err != nil {
		return err
	}
	return httpx.OK(c, result)
}

// ImportProducts validates again inside the write transaction. Locking the
// store serializes imports (including stores without products); row locks
// coordinate existing product edits and stock reservations. Unique keyword
// constraints still arbitrate concurrent individual product creation.
func (s *Service) ImportProducts(ctx context.Context, input ImportProductsInput, apply bool) (*ImportProductsResponse, error) {
	req := ImportProductsRequest{MatchBy: input.MatchBy, Rows: input.Rows, Revision: input.Revision}
	storeID := input.StoreID
	if err := req.Validate(); err != nil {
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, err.Error())
	}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting product import: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if apply {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM stores WHERE id=$1 FOR NO KEY UPDATE`, storeID.String()).Scan(&id); err != nil {
			return nil, fmt.Errorf("locking import store: %w", err)
		}
		// Lock only candidates in this batch so unrelated live sales can keep
		// reserving stock. NO KEY UPDATE above allows normal store FK checks.
		codes, skus := []string{}, []string{}
		for _, row := range req.Rows {
			if code, err := domain.NewKeyword(strings.TrimSpace(row.Keyword)); err == nil {
				codes = append(codes, code.String())
			}
			if row.SKU != nil {
				skus = append(skus, strings.TrimSpace(*row.SKU))
			}
		}
		// Consume and close before querying through the same connection.
		locked, err := tx.Query(ctx, `SELECT id FROM products WHERE store_id=$1
            AND (keyword=ANY($2::text[]) OR sku=ANY($3::text[])) ORDER BY id FOR UPDATE`, storeID.String(), codes, skus)
		if err != nil {
			return nil, fmt.Errorf("locking imported catalog: %w", err)
		}
		for locked.Next() {
		}
		err = locked.Err()
		locked.Close()
		if err != nil {
			return nil, fmt.Errorf("locking imported catalog: %w", err)
		}
	}
	repo := &Repository{q: s.repo.q.WithTx(tx), pool: s.repo.pool}
	products, err := repo.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	plan := planProductImport(storeID, req, products)
	if !apply {
		return &plan.response, nil
	}
	if req.Revision == "" || req.Revision != plan.response.Revision {
		return nil, httpx.DomainError(409, httpx.CodeProductImportChanged, "O catálogo ou a planilha mudou. Gere uma nova prévia antes de importar. Se houve falha de conexão, confira se os produtos já foram importados.")
	}
	if plan.response.Invalid > 0 {
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, "Corrija as linhas inválidas antes de importar. Nenhum produto foi alterado.")
	}
	for i, p := range plan.products {
		if plan.existing[i] {
			row := req.Rows[i]
			_, err = repo.Update(ctx, p, updatePreservation{image: true, shipping: true, stock: row.Stock == nil || p.IsFromExternalSource(), sku: row.SKU == nil, barcode: row.Barcode == nil})
		} else {
			err = repo.Save(ctx, p)
			if err == nil && !p.Active() {
				_, err = repo.Update(ctx, p, updatePreservation{})
			}
		}
		if err != nil {
			return nil, fmt.Errorf("saving imported product at row %d: %w", req.Rows[i].Row, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing product import: %w", err)
	}
	plan.response.Applied = true
	return &plan.response, nil
}

func planProductImport(storeID vo.StoreID, req ImportProductsRequest, catalog []*domain.Product) importPlan {
	plan := importPlan{response: ImportProductsResponse{Rows: make([]ImportProductResult, 0, len(req.Rows))}}
	byCode := map[string]*domain.Product{}
	bySKU := map[string][]*domain.Product{}
	reserved := map[string]bool{}
	maxCode := domain.KeywordMin - 1
	for _, p := range catalog {
		byCode[p.Keyword().String()] = p
		if sku := p.Shipping().SKU; sku != "" {
			bySKU[sku] = append(bySKU[sku], p)
		}
		reserved[p.Keyword().String()] = true
		if p.Keyword().Int() > maxCode {
			maxCode = p.Keyword().Int()
		}
	}
	// Reserve explicit codes before allocating codes for SKU-only rows.
	for _, row := range req.Rows {
		if kw, err := domain.NewKeyword(row.Keyword); err == nil {
			reserved[kw.String()] = true
		}
	}
	seen := map[string]bool{}
	seenProducts := map[string]bool{}
	seenCodes := map[string]bool{}
	revision := sha256.New()
	requestSnapshot := req
	requestSnapshot.Revision = ""
	_ = json.NewEncoder(revision).Encode(struct {
		Store   string
		Request ImportProductsRequest
	}{storeID.String(), requestSnapshot})
	for _, row := range req.Rows {
		result := ImportProductResult{Row: row.Row, Action: "create", Errors: []string{}, Warnings: []string{}}
		code := strings.TrimSpace(row.Keyword)
		if code != "" {
			kw, err := domain.NewKeyword(code)
			if err != nil {
				result.Errors = append(result.Errors, "Código LiveCart deve ser um número entre 1000 e 9999.")
			} else {
				code = kw.String()
			}
		}
		key := code
		if req.MatchBy == "sku" {
			key = ""
			if row.SKU != nil {
				key = strings.TrimSpace(*row.SKU)
			}
		}
		if key == "" {
			result.Errors = append(result.Errors, "Informe o identificador escolhido (código LiveCart ou SKU).")
		}
		if seen[key] {
			result.Errors = append(result.Errors, "Identificador repetido na planilha.")
		}
		seen[key] = true
		var existing *domain.Product
		if req.MatchBy == "keyword" {
			existing = byCode[key]
		} else {
			matches := bySKU[key]
			if len(matches) > 1 {
				result.Errors = append(result.Errors, "SKU pertence a mais de um produto. Use o código LiveCart.")
			} else if len(matches) == 1 {
				existing = matches[0]
			}
		}
		if existing != nil {
			result.Action = "update"
			if code != "" && code != existing.Keyword().String() {
				result.Errors = append(result.Errors, "SKU e código LiveCart apontam para produtos diferentes.")
			}
			if seenProducts[existing.ID().String()] {
				result.Errors = append(result.Errors, "Produto repetido na planilha.")
			}
			seenProducts[existing.ID().String()] = true
			code = existing.Keyword().String()
			// Values as well as timestamps: inventory writers need not touch updated_at.
			_ = json.NewEncoder(revision).Encode(struct {
				ID, Version, Name, SKU, Barcode, Source string
				Price                                   int64
				Stock                                   int
				Active                                  bool
			}{existing.ID().String(), existing.UpdatedAt().String(), existing.Name(), existing.Shipping().SKU, existing.Shipping().Barcode, existing.ExternalSource().String(), existing.Price().Cents(), existing.Stock(), existing.Active()})
		} else if code != "" && byCode[code] != nil {
			result.Errors = append(result.Errors, "Código LiveCart já utilizado por outro produto.")
		}
		if existing == nil && code == "" {
			for maxCode < domain.KeywordMax {
				maxCode++
				candidate := fmt.Sprintf("%04d", maxCode)
				if !reserved[candidate] {
					code = candidate
					reserved[code] = true
					break
				}
			}
			if code == "" {
				result.Errors = append(result.Errors, "Não há códigos LiveCart disponíveis.")
			}
		}
		if code != "" && seenCodes[code] {
			result.Errors = append(result.Errors, "Código LiveCart repetido na planilha.")
		}
		seenCodes[code] = true
		result.Keyword = code
		fmt.Fprintln(revision, code)
		p, errs, warnings := mergeImportProduct(storeID, code, row, existing)
		result.Errors = append(result.Errors, errs...)
		result.Warnings = append(result.Warnings, warnings...)
		if p != nil {
			result.Name = p.Name()
			result.Price = p.Price().Cents()
			result.Stock = p.Stock()
		}
		if len(result.Errors) > 0 {
			result.Action = "invalid"
			plan.response.Invalid++
		} else if existing != nil {
			plan.response.Updated++
		} else {
			plan.response.Created++
		}
		plan.response.Rows = append(plan.response.Rows, result)
		plan.products = append(plan.products, p)
		plan.existing = append(plan.existing, existing != nil)
	}
	plan.response.Revision = hex.EncodeToString(revision.Sum(nil))
	return plan
}

func mergeImportProduct(storeID vo.StoreID, code string, row ImportProductRow, existing *domain.Product) (*domain.Product, []string, []string) {
	errs, warnings := []string{}, []string{}
	name, stock, price, active := "", 0, int64(0), true
	shipping := domain.ShippingProfile{}
	if existing != nil {
		name = existing.Name()
		stock = existing.Stock()
		price = existing.Price().Cents()
		active = existing.Active()
		shipping = existing.Shipping()
	}
	if row.Row < 1 {
		errs = append(errs, "Número da linha inválido.")
	}
	if row.Name != nil {
		name = strings.TrimSpace(*row.Name)
	}
	if row.Price != nil {
		price = *row.Price
	} else if existing == nil {
		errs = append(errs, "Preço é obrigatório para novos produtos (zero é permitido).")
	}
	if row.Active != nil {
		active = *row.Active
	}
	if row.Stock != nil {
		if *row.Stock < 0 || *row.Stock > math.MaxInt32 {
			errs = append(errs, "Estoque deve ser inteiro, entre 0 e 2147483647.")
		}
		if existing != nil && existing.IsFromExternalSource() {
			warnings = append(warnings, "Estoque preservado: este produto é sincronizado pelo ERP.")
		} else {
			stock = *row.Stock
		}
	}
	if row.SKU != nil {
		shipping.SKU = strings.TrimSpace(*row.SKU)
	}
	if row.Barcode != nil {
		shipping.Barcode = strings.TrimSpace(*row.Barcode)
	}
	if len(shipping.SKU) > 100 || len(shipping.Barcode) > 100 {
		errs = append(errs, "SKU e código de barras devem ter até 100 caracteres.")
	}
	if name == "" || len([]rune(name)) > 200 {
		errs = append(errs, "Nome deve ter entre 1 e 200 caracteres.")
	}
	if price < 0 || price > 9007199254740991 {
		errs = append(errs, "Preço inválido; informe um valor positivo ou zero.")
	}
	if len(errs) > 0 {
		return nil, errs, warnings
	}
	money, _ := vo.NewMoney(price)
	if existing != nil {
		// Repository objects are request-local; retained identifiers/group/source and
		// shipping are never replaced with invented provider bindings.
		copy := *existing
		existing = &copy
		if err := existing.UpdateDetails(name, money, existing.ImageURL(), stock, active, shipping); err != nil {
			return nil, []string{err.Error()}, warnings
		}
		if existing.IsFromExternalSource() {
			warnings = append(warnings, "Dados atualizados apenas no LiveCart; uma sincronização do ERP pode substituí-los.")
		}
		return existing, errs, warnings
	}
	kw, err := domain.NewKeyword(code)
	if err != nil {
		return nil, []string{"Código LiveCart inválido."}, warnings
	}
	p, err := domain.NewProduct(storeID, name, "", domain.ExternalSourceManual, kw, money, "", stock, shipping)
	if err != nil {
		return nil, []string{err.Error()}, warnings
	}
	if !active {
		p.Deactivate()
	}
	return p, errs, warnings
}
