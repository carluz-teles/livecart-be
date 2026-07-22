package productgroup

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	productpkg "livecart/apps/api/internal/product"
)

// hasFieldKey confere se a chave JSON do campo ofensor aparece no erro ozzo.
func hasFieldKey(t *testing.T, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("esperava erro contendo a chave %q, veio nil", key)
	}
	verrs, ok := err.(validation.Errors)
	if !ok {
		return
	}
	if _, found := verrs[key]; !found {
		t.Fatalf("esperava a chave %q no erro, veio: %v", key, verrs)
	}
}

// longStr devolve uma string de n bytes 'a' para exercitar limites de Length.
func longStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func validOptionRequest() OptionRequest {
	return OptionRequest{
		Name:   "Cor",
		Values: []string{"Azul", "Verde"},
	}
}

func TestOptionRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*OptionRequest)
		wantErr bool
		key     string
	}{
		{"válido", func(*OptionRequest) {}, false, ""},
		{"name vazio", func(r *OptionRequest) { r.Name = "" }, true, "name"},
		{"name > 50", func(r *OptionRequest) { r.Name = longStr(51) }, true, "name"},
		{"values vazio", func(r *OptionRequest) { r.Values = []string{} }, true, "values"},
		{"values com elemento vazio", func(r *OptionRequest) { r.Values = []string{""} }, true, "values"},
		{"values com elemento > 80", func(r *OptionRequest) { r.Values = []string{longStr(81)} }, true, "values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validOptionRequest()
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

func validVariantRequest() VariantRequest {
	return VariantRequest{
		OptionValues: []string{"Azul"},
		Price:        1990,
		Stock:        10,
		SKU:          "SKU-1",
		Keyword:      "abcd",
		Images:       []string{"https://cdn/x.jpg"},
		Shipping:     productpkg.ShippingProfileDTO{PackageFormat: "box"},
	}
}

func TestVariantRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*VariantRequest)
		wantErr bool
		key     string
	}{
		{"válido", func(*VariantRequest) {}, false, ""},
		{"optionValues vazio", func(r *VariantRequest) { r.OptionValues = []string{} }, true, "optionValues"},
		{"optionValues com elemento vazio", func(r *VariantRequest) { r.OptionValues = []string{""} }, true, "optionValues"},
		{"price < 0", func(r *VariantRequest) { r.Price = -1 }, true, "price"},
		{"stock < 0", func(r *VariantRequest) { r.Stock = -1 }, true, "stock"},
		{"sku > 100", func(r *VariantRequest) { r.SKU = longStr(101) }, true, "sku"},
		{"keyword tamanho != 4", func(r *VariantRequest) { r.Keyword = "abc" }, true, "keyword"},
		{"images com elemento vazio", func(r *VariantRequest) { r.Images = []string{""} }, true, "images"},
		{"keyword vazio é permitido (When)", func(r *VariantRequest) { r.Keyword = "" }, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validVariantRequest()
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

func validCreateGroupRequest() CreateGroupRequest {
	return CreateGroupRequest{
		Name:           "Camiseta Básica",
		ExternalSource: "manual",
		Options:        []OptionRequest{validOptionRequest()},
		GroupImages:    []string{"https://cdn/g.jpg"},
		Variants:       []VariantRequest{validVariantRequest()},
	}
}

func TestCreateGroupRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreateGroupRequest)
		wantErr bool
		key     string
	}{
		{"válido", func(*CreateGroupRequest) {}, false, ""},
		{"externalSource vazio é permitido (When)", func(r *CreateGroupRequest) { r.ExternalSource = "" }, false, ""},
		{"name vazio", func(r *CreateGroupRequest) { r.Name = "" }, true, "name"},
		{"name > 200", func(r *CreateGroupRequest) { r.Name = longStr(201) }, true, "name"},
		{"externalSource inválido", func(r *CreateGroupRequest) { r.ExternalSource = "magento" }, true, "externalSource"},
		{"options vazio", func(r *CreateGroupRequest) { r.Options = []OptionRequest{} }, true, "options"},
		{"option aninhada inválida", func(r *CreateGroupRequest) { r.Options = []OptionRequest{{Name: "", Values: []string{"x"}}} }, true, "options"},
		{"groupImages com elemento vazio", func(r *CreateGroupRequest) { r.GroupImages = []string{""} }, true, "groupImages"},
		{"variants vazio", func(r *CreateGroupRequest) { r.Variants = []VariantRequest{} }, true, "variants"},
		{"variant aninhada inválida", func(r *CreateGroupRequest) {
			bad := validVariantRequest()
			bad.Price = -1
			r.Variants = []VariantRequest{bad}
		}, true, "variants"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validCreateGroupRequest()
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

func TestUpdateGroupRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     UpdateGroupRequest
		wantErr bool
		key     string
	}{
		{"válido", UpdateGroupRequest{Name: "Novo Nome"}, false, ""},
		{"name vazio", UpdateGroupRequest{Name: ""}, true, "name"},
		{"name > 200", UpdateGroupRequest{Name: longStr(201)}, true, "name"},
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

func TestAddImageRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     AddImageRequest
		wantErr bool
		key     string
	}{
		{"válido", AddImageRequest{URL: "https://cdn/x.jpg", Position: 0}, false, ""},
		{"url vazio", AddImageRequest{URL: "", Position: 0}, true, "url"},
		{"url inválido", AddImageRequest{URL: "not a url", Position: 0}, true, "url"},
		{"position < 0", AddImageRequest{URL: "https://cdn/x.jpg", Position: -1}, true, "position"},
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
