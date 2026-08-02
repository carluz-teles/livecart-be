package integration

// Portão sintático do agendamento e limites de mídia do Instagram.
//
// Sem banco de propósito: são as duas camadas que precisam recusar ANTES de o
// arquivo subir para o storage e ficar retido por dias.

import (
	"strings"
	"testing"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

func TestScheduleInstagramPublishRequestValidate(t *testing.T) {
	when := time.Now().Add(2 * time.Hour)
	minutes := func(n int) *int { return &n }

	base := func() ScheduleInstagramPublishRequest {
		return ScheduleInstagramPublishRequest{
			MediaKind:    "post",
			ScheduledFor: &when,
			ProductIDs:   []string{"11111111-1111-1111-1111-111111111111"},
		}
	}

	cases := []struct {
		name  string
		mut   func(*ScheduleInstagramPublishRequest)
		field string // json key esperada em validation.Errors; "" = deve passar
	}{
		{"caso valido", func(*ScheduleInstagramPublishRequest) {}, ""},
		{"sem especie", func(r *ScheduleInstagramPublishRequest) { r.MediaKind = "" }, "mediaKind"},
		{"especie desconhecida", func(r *ScheduleInstagramPublishRequest) { r.MediaKind = "carousel" }, "mediaKind"},
		{"sem data", func(r *ScheduleInstagramPublishRequest) { r.ScheduledFor = nil }, "scheduledFor"},
		{"sem produto", func(r *ScheduleInstagramPublishRequest) { r.ProductIDs = nil }, "productIds"},
		// O piso de 15 min do prazo do carrinho (000104) vale aqui também: o
		// evento nasce no disparo com este valor e o CHECK do banco o recusaria
		// como 500 em vez de 422 com campo.
		{"prazo abaixo do piso", func(r *ScheduleInstagramPublishRequest) { r.CartExpirationMinutes = minutes(5) }, "cartExpirationMinutes"},
		// Zero explícito é o caso que o ozzo pula quando Min vem sem Required —
		// o gotcha da convenção. Com o par, ele é recusado.
		{"prazo zero explicito", func(r *ScheduleInstagramPublishRequest) { r.CartExpirationMinutes = minutes(0) }, "cartExpirationMinutes"},
		{"prazo valido", func(r *ScheduleInstagramPublishRequest) { r.CartExpirationMinutes = minutes(60) }, ""},
		{"prazo omitido usa o da loja", func(r *ScheduleInstagramPublishRequest) { r.CartExpirationMinutes = nil }, ""},
		{"teto por item zero explicito", func(r *ScheduleInstagramPublishRequest) { r.CartMaxQuantityPerItem = minutes(0) }, "cartMaxQuantityPerItem"},
		{"legenda longa demais", func(r *ScheduleInstagramPublishRequest) { r.Caption = strings.Repeat("a", 2201) }, "caption"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mut(&req)
			err := req.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("esperava aceitar, veio: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("esperava recusar e passou")
			}
			errs, ok := err.(validation.Errors)
			if !ok {
				t.Fatalf("esperava validation.Errors, veio %T: %v", err, err)
			}
			if _, found := errs[tc.field]; !found {
				t.Fatalf("esperava erro no campo %q, veio %v", tc.field, errs)
			}
		})
	}
}

func TestValidateInstagramAsset(t *testing.T) {
	const mb = 1024 * 1024

	cases := []struct {
		name        string
		kind        string
		contentType string
		size        int64
		ok          bool
	}{
		{"post jpeg no limite", "post", "image/jpeg", 8 * mb, true},
		{"post jpeg acima do limite", "post", "image/jpeg", 8*mb + 1, false},
		{"post com video", "post", "video/mp4", mb, false},
		{"reel mp4", "reel", "video/mp4", 200 * mb, true},
		{"reel acima do limite", "reel", "video/mp4", 301 * mb, false},
		{"reel com imagem", "reel", "image/jpeg", mb, false},
		{"story foto", "story", "image/jpeg", mb, true},
		{"story video", "story", "video/quicktime", 50 * mb, true},
		// O teto do story é MENOR que o do reel (100MB x 300MB): quando os
		// limites viviam repetidos em três handlers, era exatamente esse tipo
		// de diferença que divergia.
		{"story video acima do limite", "story", "video/mp4", 101 * mb, false},
		{"story com pdf", "story", "application/pdf", mb, false},
		{"especie desconhecida", "carousel", "image/jpeg", mb, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstagramAsset(tc.kind, tc.contentType, tc.size)
			if tc.ok && err != nil {
				t.Fatalf("esperava aceitar, veio: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("esperava recusar e passou")
			}
		})
	}
}
