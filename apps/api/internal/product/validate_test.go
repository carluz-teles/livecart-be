package product

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// ptrInt / ptrInt64 são helpers locais (nomes exclusivos p/ não colidir com
// stock_sync_test.go) para montar os ponteiros opcionais das DTOs.
func ptrInt(v int) *int       { return &v }
func ptrInt64(v int64) *int64 { return &v }

// hasFieldKey confere se a chave JSON do campo ofensor aparece no erro ozzo.
func hasFieldKey(t *testing.T, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("esperava erro contendo a chave %q, veio nil", key)
	}
	verrs, ok := err.(validation.Errors)
	if !ok {
		// Alguns erros aninhados podem não ser validation.Errors; err != nil já basta.
		return
	}
	if _, found := verrs[key]; !found {
		t.Fatalf("esperava a chave %q no erro, veio: %v", key, verrs)
	}
}

func validShippingDTO() ShippingProfileDTO {
	return ShippingProfileDTO{
		WeightGrams:         ptrInt(500),
		HeightCm:            ptrInt(10),
		WidthCm:             ptrInt(20),
		LengthCm:            ptrInt(30),
		SKU:                 "SKU-123",
		PackageFormat:       "box",
		InsuranceValueCents: ptrInt64(1000),
	}
}

func TestShippingProfileDTOValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ShippingProfileDTO)
		wantErr bool
		key     string
	}{
		{"válido", func(*ShippingProfileDTO) {}, false, ""},
		{"weight < 1", func(d *ShippingProfileDTO) { d.WeightGrams = ptrInt(-1) }, true, "weightGrams"},
		{"height < 1", func(d *ShippingProfileDTO) { d.HeightCm = ptrInt(-1) }, true, "heightCm"},
		{"width < 1", func(d *ShippingProfileDTO) { d.WidthCm = ptrInt(-1) }, true, "widthCm"},
		{"length < 1", func(d *ShippingProfileDTO) { d.LengthCm = ptrInt(-1) }, true, "lengthCm"},
		{"sku > 100", func(d *ShippingProfileDTO) { d.SKU = longStr(101) }, true, "sku"},
		{"packageFormat inválido", func(d *ShippingProfileDTO) { d.PackageFormat = "envelope" }, true, "packageFormat"},
		{"insurance < 0", func(d *ShippingProfileDTO) { d.InsuranceValueCents = ptrInt64(-1) }, true, "insuranceValueCents"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validShippingDTO()
			tc.mutate(&d)
			err := d.Validate()
			if tc.wantErr {
				hasFieldKey(t, err, tc.key)
			} else if err != nil {
				t.Fatalf("esperava nil, veio: %v", err)
			}
		})
	}
}

func validCreateProductRequest() CreateProductRequest {
	return CreateProductRequest{
		Name:           "Camiseta Básica",
		ExternalSource: "manual",
		Stock:          10,
		GroupID:        "550e8400-e29b-41d4-a716-446655440000",
		Shipping:       validShippingDTO(),
		Images:         []string{"https://cdn/x.jpg"},
	}
}

func TestCreateProductRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreateProductRequest)
		wantErr bool
		key     string
	}{
		{"válido", func(*CreateProductRequest) {}, false, ""},
		{"name vazio", func(r *CreateProductRequest) { r.Name = "" }, true, "name"},
		{"name > 200", func(r *CreateProductRequest) { r.Name = longStr(201) }, true, "name"},
		{"externalSource vazio", func(r *CreateProductRequest) { r.ExternalSource = "" }, true, "externalSource"},
		{"externalSource inválido", func(r *CreateProductRequest) { r.ExternalSource = "magento" }, true, "externalSource"},
		{"stock < 0", func(r *CreateProductRequest) { r.Stock = -1 }, true, "stock"},
		{"groupId não-uuid", func(r *CreateProductRequest) { r.GroupID = "not-a-uuid" }, true, "groupId"},
		{"shipping aninhado inválido", func(r *CreateProductRequest) { r.Shipping.PackageFormat = "envelope" }, true, "shipping"},
		{"images com elemento vazio", func(r *CreateProductRequest) { r.Images = []string{""} }, true, "images"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validCreateProductRequest()
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr {
				hasFieldKey(t, err, tc.key)
			} else if err != nil {
				t.Fatalf("esperava nil, veio: %v", err)
			}
		})
	}
}

func TestAddProductImageRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     AddProductImageRequest
		wantErr bool
		key     string
	}{
		{"válido", AddProductImageRequest{URL: "https://cdn/x.jpg", Position: 0}, false, ""},
		{"url vazio", AddProductImageRequest{URL: "", Position: 0}, true, "url"},
		{"url inválido", AddProductImageRequest{URL: "not a url", Position: 0}, true, "url"},
		{"position < 0", AddProductImageRequest{URL: "https://cdn/x.jpg", Position: -1}, true, "position"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr {
				hasFieldKey(t, err, tc.key)
			} else if err != nil {
				t.Fatalf("esperava nil, veio: %v", err)
			}
		})
	}
}

func validUpdateProductRequest() UpdateProductRequest {
	return UpdateProductRequest{
		Name:     "Camiseta Atualizada",
		Stock:    5,
		Shipping: validShippingDTO(),
	}
}

func TestUpdateProductRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*UpdateProductRequest)
		wantErr bool
		key     string
	}{
		{"válido", func(*UpdateProductRequest) {}, false, ""},
		{"name vazio", func(r *UpdateProductRequest) { r.Name = "" }, true, "name"},
		{"name > 200", func(r *UpdateProductRequest) { r.Name = longStr(201) }, true, "name"},
		{"stock < 0", func(r *UpdateProductRequest) { r.Stock = -1 }, true, "stock"},
		{"shipping aninhado inválido", func(r *UpdateProductRequest) { r.Shipping.WeightGrams = ptrInt(-1) }, true, "shipping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validUpdateProductRequest()
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr {
				hasFieldKey(t, err, tc.key)
			} else if err != nil {
				t.Fatalf("esperava nil, veio: %v", err)
			}
		})
	}
}

// longStr devolve uma string de n runes 'a' para exercitar limites de Length.
func longStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
