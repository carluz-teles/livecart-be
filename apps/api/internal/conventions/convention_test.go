package conventions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestEveryRequestDTOHasValidate is the convention ratchet for the HTTP-domain
// standard (see skill api-domain-convention): every inbound *Request DTO must
// carry a `Validate() error` (the syntactic ozzo gate) so BindAndValidate has a
// contract to call.
//
// The sweep is not finished, and some `*Request` types are outbound payloads to
// external APIs (SmartEnvios/MelhorEnvio) that never validate — so we freeze the
// current set of Request-without-Validate as a BASELINE and only fail when the
// set GROWS. A new *Request struct must ship with Validate() or a reviewer must
// consciously add it to the baseline. The test also fails if a baseline entry
// gains Validate (remove it — the ratchet tightens) or disappears (clean it up),
// so the list can only shrink and never rots.
func TestEveryRequestDTOHasValidate(t *testing.T) {
	internalDir := internalRoot(t)

	requests := map[string]token.Position{} // pkg.Type -> declaration position
	validated := map[string]bool{}          // pkg.Type with a Validate() error method

	fset := token.NewFileSet()
	err := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		pkg := file.Name.Name
		for _, decl := range file.Decls {
			switch dcl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range dcl.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
						continue
					}
					if strings.HasSuffix(ts.Name.Name, "Request") {
						requests[pkg+"."+ts.Name.Name] = fset.Position(ts.Name.Pos())
					}
				}
			case *ast.FuncDecl:
				if dcl.Recv == nil || dcl.Name.Name != "Validate" {
					continue
				}
				if recv := receiverTypeName(dcl); recv != "" {
					validated[pkg+"."+recv] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", internalDir, err)
	}

	// New violations: a *Request without Validate that isn't a known exception.
	var newViolations []string
	for key, pos := range requests {
		if validated[key] || baselineWithoutValidate[key] {
			continue
		}
		newViolations = append(newViolations, key+"  ("+pos.String()+")")
	}
	sort.Strings(newViolations)
	if len(newViolations) > 0 {
		t.Errorf("%d novo(s) *Request sem Validate() error — adicione o gate ozzo (skill api-domain-convention) ou, se for payload de API externa, inclua no baseline:\n  %s",
			len(newViolations), strings.Join(newViolations, "\n  "))
	}

	// Ratchet hygiene: baseline may only shrink.
	var toRemove []string
	for key := range baselineWithoutValidate {
		if _, stillExists := requests[key]; !stillExists {
			toRemove = append(toRemove, key+"  (não existe mais — remover do baseline)")
			continue
		}
		if validated[key] {
			toRemove = append(toRemove, key+"  (agora tem Validate() — remover do baseline)")
		}
	}
	sort.Strings(toRemove)
	if len(toRemove) > 0 {
		t.Errorf("baseline desatualizado (%d) — a lista só encolhe:\n  %s",
			len(toRemove), strings.Join(toRemove, "\n  "))
	}
}

// receiverTypeName returns the bare receiver type name (deref pointer) of a
// method, or "" if it can't be determined.
func receiverTypeName(fn *ast.FuncDecl) string {
	if len(fn.Recv.List) == 0 {
		return ""
	}
	switch e := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// internalRoot resolves apps/api/internal from this test file's location so the
// walk works regardless of the working directory `go test` runs from.
func internalRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	// thisFile = .../apps/api/internal/conventions/convention_test.go
	return filepath.Dir(filepath.Dir(thisFile))
}

// baselineWithoutValidate freezes the *Request structs (keyed by package.Type)
// that lacked Validate() as of 2026-07-22. Two kinds live here: inbound DTOs of
// domains not yet migrated to the request-flow convention, and outbound payloads
// to external shipping APIs (SmartEnvios `se*` / MelhorEnvio `me*`) that are not
// HTTP request bodies. RATCHET: only remove entries, never add — a new inbound
// *Request should carry Validate() instead.
var baselineWithoutValidate = map[string]bool{
	// checkout — domínio não migrado (fluxo de carrinho/pagamento público)
	"checkout.AddCartItemRequest":            true,
	"checkout.GenerateCheckoutRequest":       true,
	"checkout.GeneratePixRequest":            true,
	"checkout.ProcessCardPaymentRequest":     true,
	"checkout.SelectShippingMethodRequest":   true,
	"checkout.ShippingQuoteRequest":          true,
	"checkout.UpdateCartItemQuantityRequest": true,

	// integration — domínio não migrado (conexões ERP/pagamento/whatsapp)
	"integration.AddressRequest":                true,
	"integration.AttachShippingInvoiceRequest":  true,
	"integration.ConnectSmartEnviosRequest":     true,
	"integration.ConnectWhatsAppRequest":        true,
	"integration.CreateCheckoutRequest":         true,
	"integration.CreateInstagramPostRequest":    true,
	"integration.CreateIntegrationRequest":      true,
	"integration.CreateShippingShipmentRequest": true,
	"integration.GenerateShippingLabelsRequest": true,
	"integration.ImportERPProductRequest":       true,
	"integration.RefundRequest":                 true,
	"integration.SendWhatsAppTestRequest":       true,
	"integration.ShipmentItemRequest":           true,
	"integration.TrackShippingRequest":          true,
	"integration.UpdateIntegrationRequest":      true,
	"integration.UpdatePriorityRequest":         true,
	"integration.VerifyWhatsAppRequest":         true,

	// live — domínio não migrado (eventos/lives/posts)
	"live.AddPlatformRequest":         true,
	"live.BulkEventProductsRequest":   true,
	"live.CreateEventRequest":         true,
	"live.CreateLiveRequest":          true,
	"live.CreatePostRequest":          true,
	"live.CreateSessionRequest":       true,
	"live.EndEventRequest":            true,
	"live.EndLiveRequest":             true,
	"live.EventProductRequest":        true,
	"live.EventUpsellRequest":         true,
	"live.SetActiveProductRequest":    true,
	"live.SetProcessingPausedRequest": true,
	"live.UpdateLiveRequest":          true,

	// notification — domínio não migrado (settings/templates de comunicação)
	"notification.PreviewTemplateRequest":             true,
	"notification.SendTestEmailRequest":               true,
	"notification.SendTestRequest":                    true,
	"notification.UpdateEmailTemplateSettingsRequest": true,
	"notification.UpdateSettingsRequest":              true,
	"notification.UpdateTemplateSettingsRequest":      true,

	// providers / shipping — payloads de REQUEST para APIs EXTERNAS
	// (Correios/transportadoras, SmartEnvios `se*`, MelhorEnvio `me*`).
	// NÃO são bodies HTTP de entrada; nunca terão Validate().
	"providers.AttachInvoiceRequest":    true,
	"providers.CreateShipmentRequest":   true,
	"providers.GenerateLabelsRequest":   true,
	"providers.QuoteRequest":            true,
	"providers.TrackShipmentRequest":    true,
	"providers.UploadInvoiceXMLRequest": true,
	"shipping.meCartCreateRequest":      true,
	"shipping.meProductRequest":         true,
	"shipping.seDcCreateRequest":        true,
	"shipping.seLabelsRequest":          true,
	"shipping.seQuoteRequest":           true,
	"shipping.seTrackingRequest":        true,
}
