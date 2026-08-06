package conventions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoNewRawHttpxThrows is the convention ratchet for the error-system
// initiative (docs/error-system-plan.md, Phase D). It AST-walks internal/** for
// calls to the seven RAW httpx.Err* constructors — the ones that raise a
// *ServiceError WITHOUT a stable FE Code (lib/httpx/errors.go) — and freezes the
// current set as a BASELINE, failing only when the set GROWS.
//
// The migration to coded errors (httpx.DomainError / httpx.WithCode) is
// deliberately incremental: rewriting all ~335 sites at once is a big-bang no one
// wants. So instead of banning raw throws outright we ratchet: a NEW raw
// httpx.Err* must ship as a coded DomainError, or a reviewer must consciously add
// it to baselineRawThrows. The test also fails when a baseline entry disappears
// (a throw was coded or deleted — clean it up) so the list only shrinks and never
// rots. Same two-direction shape as TestEveryRequestDTOHasValidate
// (convention_test.go:76-105), whose internalRoot/receiverTypeName it reuses.
//
// DomainError/WithCode are DIFFERENT selectors (or wrap an Err* value), so a
// coded error never matches this walk — by construction, already-coded throws do
// not count against the baseline.
func TestNoNewRawHttpxThrows(t *testing.T) {
	found := collectRawHttpxThrows(t)

	if grew := newRawThrowViolations(found, baselineRawThrows); len(grew) > 0 {
		t.Errorf("%d novo(s) throw httpx.Err* CRU (sem Code) — use httpx.DomainError(status, httpx.Code…, msg), ou httpx.WithCode(err, httpx.Code…) num Err* existente; se for not-found/validação/config, adicione conscientemente ao baselineRawThrows:\n  %s",
			len(grew), strings.Join(grew, "\n  "))
	}

	if stale := staleBaselineEntries(found, baselineRawThrows); len(stale) > 0 {
		t.Errorf("baselineRawThrows desatualizado (%d) — a lista SÓ ENCOLHE; remova as entradas que sumiram (throw migrado p/ Code ou apagado):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// TestRawHttpxThrowRatchet_Logic proves the ratchet reacts in both directions
// WITHOUT adding a real raw throw to production code: it feeds synthetic found/
// baseline sets through the same comparison helpers the real test uses.
func TestRawHttpxThrowRatchet_Logic(t *testing.T) {
	const syntheticKey = `synthetic.Service.DoThing:"boom"`

	t.Run("throw novo fora do baseline vira violação acionável", func(t *testing.T) {
		found := map[string]token.Position{
			syntheticKey: {Filename: "synthetic.go", Line: 1, Column: 1},
		}
		got := newRawThrowViolations(found, map[string]bool{})
		if len(got) != 1 {
			t.Fatalf("esperava 1 violação para chave fora do baseline, obteve %d: %v", len(got), got)
		}
		if !strings.Contains(got[0], "synthetic.Service.DoThing") {
			t.Errorf("mensagem deve citar o site sintético, obteve %q", got[0])
		}
	})

	t.Run("throw presente no baseline não viola", func(t *testing.T) {
		found := map[string]token.Position{syntheticKey: {Filename: "synthetic.go", Line: 1}}
		if got := newRawThrowViolations(found, map[string]bool{syntheticKey: true}); len(got) != 0 {
			t.Errorf("chave presente no baseline não deve violar, obteve %v", got)
		}
	})

	t.Run("entrada do baseline que sumiu é sinalizada (só encolhe)", func(t *testing.T) {
		baseline := map[string]bool{`synthetic.Service.Gone:"x"`: true}
		if got := staleBaselineEntries(map[string]token.Position{}, baseline); len(got) != 1 {
			t.Fatalf("esperava 1 entrada stale, obteve %d: %v", len(got), got)
		}
	})
}

// rawHttpxThrowSelectors is the FROZEN set of the seven httpx.Err* constructors
// that build a *ServiceError with no FE Code (lib/httpx/errors.go:90-96). Freezing
// all seven — including ErrNotFound (404) and ErrInternal (500) — is deliberate:
// a NEW not-found or internal throw also trips the ratchet, so a reviewer decides
// via the baseline whether it stays uncoded. See baselineRawThrows for policy.
var rawHttpxThrowSelectors = map[string]bool{
	"ErrBadRequest":    true,
	"ErrUnprocessable": true,
	"ErrConflict":      true,
	"ErrForbidden":     true,
	"ErrGone":          true,
	"ErrNotFound":      true,
	"ErrInternal":      true,
}

// newRawThrowViolations returns baseline-relative growth: raw httpx.Err* sites
// present in the walk but absent from the baseline, each annotated with its
// source position for an actionable failure message.
func newRawThrowViolations(found map[string]token.Position, baseline map[string]bool) []string {
	var out []string
	for key, pos := range found {
		if baseline[key] {
			continue
		}
		out = append(out, key+"  ("+pos.String()+")")
	}
	sort.Strings(out)
	return out
}

// staleBaselineEntries returns baseline keys no longer found in the walk — the
// "list only shrinks" hygiene check.
func staleBaselineEntries(found map[string]token.Position, baseline map[string]bool) []string {
	var out []string
	for key := range baseline {
		if _, ok := found[key]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// collectRawHttpxThrows walks internal/** (skipping _test.go; lib/httpx lives
// outside internal/ so it is never visited) and returns every raw httpx.Err*
// call site keyed by a LINE-INDEPENDENT stable key so the baseline survives
// reformatting: "pkg.func:<string-literal>" when the first arg is a string
// literal, else "pkg.func#<ordinal>" (ordinal = position among the function's
// non-literal-arg throws, e.g. fmt.Sprintf / err.Error()). func = receiver.method
// for methods (reusing receiverTypeName), else the plain function name.
func collectRawHttpxThrows(t *testing.T) map[string]token.Position {
	t.Helper()
	internalDir := internalRoot(t)
	found := map[string]token.Position{}
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcName := fn.Name.Name
			if fn.Recv != nil {
				if recv := receiverTypeName(fn); recv != "" {
					funcName = recv + "." + fn.Name.Name
				}
			}
			prefix := pkg + "." + funcName
			nonLit := 0
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || x.Name != "httpx" || !rawHttpxThrowSelectors[sel.Sel.Name] {
					return true
				}
				var key string
				if len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						key = prefix + ":" + lit.Value
					} else {
						key = prefix + "#" + ordinalSuffix(nonLit)
						nonLit++
					}
				} else {
					key = prefix + "#" + ordinalSuffix(nonLit)
					nonLit++
				}
				if _, exists := found[key]; !exists {
					found[key] = fset.Position(call.Pos())
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", internalDir, err)
	}
	return found
}

// ordinalSuffix renders a small non-negative int without importing strconv/fmt,
// keeping the ratchet's import surface to the go/ast trio the precedent uses.
func ordinalSuffix(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// baselineRawThrows freezes the raw httpx.Err* throw sites that existed as of
// 2026-08-02 (335 sites), keyed by the stable "pkg.func:msg" / "pkg.func#n"
// scheme collectRawHttpxThrows builds.
//
// POLICY — three kinds of entry live here, all frozen, none added lightly:
//   - Permanent exclusions: not-found (ErrNotFound) and internal/config
//     (ErrInternal) and input-validation throws are legitimately code-less today
//     — the FE branches on the 4xx status, not a domain Code. These may stay.
//   - Shrinking debt: business-rule throws (422/409/403) not yet migrated to
//     httpx.DomainError. These are DEBT — the list only shrinks; migrate them
//     over time, then delete the entry (the ratchet's stale-check enforces this).
//   - Never grow: the ratchet REJECTS a new key. A new business error must ship
//     as DomainError(status, httpx.Code…, msg); only a conscious reviewer adds a
//     genuinely code-less not-found/validation/config throw here.
var baselineRawThrows = map[string]bool{
	"billing.WebhookHandler.HandleStripe:\"invalid event payload\"":             true,
	"checkout.Repository.BindCartPaymentIntegration:\"invalid cart ID\"":        true,
	"checkout.Repository.BindCartPaymentIntegration:\"invalid integration ID\"": true,
	"checkout.Repository.ClearCartPaymentIntegration:\"invalid cart ID\"":       true,
	"checkout.Repository.CreateCartItem:\"invalid cart ID\"":                    true,
	"checkout.Repository.CreateCartItem:\"invalid product ID\"":                 true,
	"checkout.Repository.DeleteCartItem:\"invalid item ID\"":                    true,
	"checkout.Repository.EnsureInitialSnapshot:\"invalid cart ID\"":             true,
	"checkout.Repository.GetCartByToken:\"carrinho não encontrado\"":            true,
	"checkout.Repository.GetCartItem:\"invalid item ID\"":                       true,
	"checkout.Repository.GetCartItem:\"item não encontrado\"":                   true,
	"checkout.Repository.GetEventProductForCart:\"invalid event ID\"":           true,
	"checkout.Repository.GetEventProductForCart:\"invalid product ID\"":         true,
	"checkout.Repository.GetEventProductForCart:\"invalid store ID\"":           true,
	"checkout.Repository.GetEventProductForCart:\"produto não encontrado\"":     true,
	"checkout.Repository.GetIntegrationByID:\"invalid integration ID\"":         true,
	"checkout.Repository.GetLatestPaidCustomerSnapshot:\"invalid cart ID\"":     true,
	"checkout.Repository.GetLatestPaidCustomerSnapshot:\"invalid store ID\"":    true,
	"checkout.Repository.GetPaymentIntegration:\"invalid store ID\"":            true,
	"checkout.Repository.GetShippingContextByToken:\"carrinho não encontrado\"": true,
	"checkout.Repository.ListCartItems:\"invalid cart ID\"":                     true,
	"checkout.Repository.ListPaymentIntegrations:\"invalid store ID\"":          true,
	"checkout.Repository.LoadEventCheckoutFlags:\"invalid event ID\"":           true,
	"checkout.Repository.LoadEventType:\"invalid event ID\"":                    true,
	"checkout.Repository.ReadCartPaymentDetails:\"invalid cart ID\"":            true,
	"checkout.Repository.ReadCartShipping:\"invalid cart ID\"":                  true,
	"checkout.Repository.ReadCouponSummary:\"invalid coupon ID\"":               true,
	"checkout.Repository.ReadShippingQuoteCache:\"invalid cart ID\"":            true,
	"checkout.Repository.RecordMutation:\"invalid cart ID\"":                    true,
	"checkout.Repository.RecordMutation:\"invalid product ID\"":                 true,
	"checkout.Repository.SaveShippingQuoteCache:\"invalid cart ID\"":            true,
	"checkout.Repository.SetCartItemQuantity:\"invalid item ID\"":               true,
	// Mesma validação de UUID da irmã acima, no wrapper que substituiu o
	// UpdateCartItem sem guarda. É parse de path param — a categoria que a
	// própria mensagem do ratchet manda registrar aqui.
	"checkout.Repository.SetCartItemSplitIfUnchanged:\"invalid item ID\"":                                                       true,
	"checkout.Repository.UpdateCartShipping:\"invalid cart ID\"":                                                                true,
	"checkout.Repository.UpdateCheckoutCustomer:\"carrinho não encontrado\"":                                                    true,
	"checkout.Repository.UpdateCheckoutCustomer:\"invalid cart ID\"":                                                            true,
	"checkout.Repository.UpdateCheckoutInfo:\"carrinho não encontrado\"":                                                        true,
	"checkout.Repository.UpdateCheckoutInfo:\"invalid cart ID\"":                                                                true,
	"checkout.Repository.UpdateCustomerEmail:\"carrinho não encontrado\"":                                                       true,
	"checkout.Repository.UpdatePaymentStatus:\"carrinho não encontrado\"":                                                       true,
	"checkout.Repository.UpdatePaymentStatus:\"invalid cart ID\"":                                                               true,
	"checkout.Repository.WriteCartCardPayment:\"invalid cart ID\"":                                                              true,
	"checkout.Service.AddCartItem#0":                                                                                            true,
	"checkout.Service.AddCartItem#1":                                                                                            true,
	"checkout.Service.AddCartItem:\"não foi possível adicionar o produto, tente novamente\"":                                    true,
	"checkout.Service.AddCartItem:\"produto não disponível neste evento\"":                                                      true,
	"checkout.Service.AddCartItem:\"produto não está ativo\"":                                                                   true,
	"checkout.Service.AddCartItem:\"quantidade deve ser pelo menos 1\"":                                                         true,
	"checkout.Service.DropFromWaitlist:\"item da fila não encontrado neste carrinho\"":                                          true,
	"checkout.Service.DropFromWaitlist:\"não foi possível sair da fila, tente novamente\"":                                      true,
	"checkout.Service.DropFromWaitlist:\"waitlist id is required\"":                                                             true,
	"checkout.Service.GeneratePix:\"provedor de pagamento indisponível, tente novamente em instantes\"":                         true,
	"checkout.Service.GetCartForCheckout:\"Este carrinho foi cancelado pela loja. Fale com a loja para receber um novo link.\"": true,
	"checkout.Service.GetCartForCheckout:\"carrinho não encontrado\"":                                                           true,
	"checkout.Service.ProcessCardPayment:\"provedor de pagamento indisponível, tente novamente em instantes\"":                  true,
	"checkout.Service.RemoveCartItem:\"não foi possível remover o item, tente novamente\"":                                      true,
	"checkout.Service.UpdateCartItemQuantity:\"não foi possível atualizar o estoque, tente novamente\"":                         true,
	"checkout.Service.UpdateCartItemQuantity:\"quantidade deve ser pelo menos 1\"":                                              true,
	"checkout.Service.loadEditableCart:\"carrinho cancelado\"":                                                                  true,
	"checkout.Service.loadEditableCart:\"carrinho já foi pago\"":                                                                true,
	"checkout.Service.loadEditableCart:\"edição do carrinho desabilitada para esta loja\"":                                      true,
	"checkout.Service.loadEditableCartItem:\"item não encontrado neste carrinho\"":                                              true,
	"checkout.Service.validateQuantityCap#0":                                                                                    true,
	"checkout.Service.validateQuantityCap#1":                                                                                    true,
	"coupon.Handler.Apply:\"token is required\"":                                                                                true,
	"coupon.Handler.Remove:\"token is required\"":                                                                               true,
	"coupon.Service.ApplyToCart:\"cart not found\"":                                                                             true,
	"coupon.Service.Create:\"code cannot be empty\"":                                                                            true,
	"coupon.Service.Create:\"validUntil must be after validFrom\"":                                                              true,
	"coupon.Service.Delete:\"coupon not found\"":                                                                                true,
	"coupon.Service.Get:\"coupon not found\"":                                                                                   true,
	"coupon.Service.RemoveFromCart:\"cart not found\"":                                                                          true,
	"coupon.Service.Update:\"coupon not found\"":                                                                                true,
	"coupon.Service.assertEventOwnership:\"event not found\"":                                                                   true,
	"coupon.computeAppliedDiscount:\"invalid coupon type\"":                                                                     true,
	"coupon.validateBusinessRules:\"fixed coupon requires valueCents > 0\"":                                                     true,
	"coupon.validateBusinessRules:\"invalid coupon type\"":                                                                      true,
	"coupon.validateBusinessRules:\"percent coupon requires percentBps > 0\"":                                                   true,
	"customer.BlockHandleRequest.ToInput:\"invalid store id\"":                                                                  true,
	"customer.Handler.GetByID:\"invalid customer id\"":                                                                          true,
	"customer.Handler.ListBlocked:\"invalid store id\"":                                                                         true,
	"customer.Handler.ListOrders:\"invalid customer id\"":                                                                       true,
	"customer.Handler.Unblock:\"invalid store id\"":                                                                             true,
	"customer.Service.BlockHandle:\"handle is required\"":                                                                       true,
	"customer.Service.GetByID#0":                                                                                                true,
	"customer.Service.UnblockHandle#0":                                                                                          true,
	"customer.Service.UnblockHandle:\"handle is required\"":                                                                     true,
	"erp.Service.fetchAndPersistCartInvoice:\"cart not found\"":                                                                 true,
	"idea.Service.Create:\"categoria inválida\"":                                                                                true,
	"idea.Service.CreateComment#0":                                                                                              true,
	"idea.Service.GetDetail#0":                                                                                                  true,
	"idea.Service.ToggleVote#0":                                                                                                 true,
	"idea.Service.ToggleVote:\"não é possível votar na própria ideia\"":                                                         true,
	"integration.Handler.UpdatePriority:\"payload inválido\"":                                                                   true,
	"integration.Handler.UpdatePriority:\"priority deve ser >= 0\"":                                                             true,
	"integration.Repository.CreateShipment:\"invalid order id\"":                                                                true,
	"integration.Repository.CreateShipment:\"invalid store id\"":                                                                true,
	"integration.Repository.GetActiveByProvider:\"active integration not found\"":                                               true,
	"integration.Repository.GetByID:\"integration not found\"":                                                                  true,
	"integration.Repository.GetByIDOnly:\"integration not found\"":                                                              true,
	"integration.Repository.GetByProvider:\"integration not found\"":                                                            true,
	"integration.Repository.GetShipmentByOrderID:\"invalid order id\"":                                                          true,
	"integration.Repository.InsertTrackingEvents:\"invalid shipment id\"":                                                       true,
	"integration.Repository.ListTrackingEvents:\"invalid shipment id\"":                                                         true,
	"integration.Repository.UpdateShipmentInvoice:\"invalid shipment id\"":                                                      true,
	"integration.Repository.UpdateShipmentLabels:\"invalid shipment id\"":                                                       true,
	"integration.Repository.UpdateShipmentStatus:\"invalid shipment id\"":                                                       true,
	"integration.Repository.getShipmentByIDForStore:\"invalid shipment id\"":                                                    true,
	"integration.Repository.getShipmentByIDForStore:\"invalid store id\"":                                                       true,
	"integration.Repository.getShipmentByIDForStore:\"shipment not found\"":                                                     true,
	"integration.Service.CancelCart:\"carrinho não pode ser cancelado: já foi pago, expirou ou já estava cancelado\"":           true,
	"integration.Service.CancelCart:\"pagamento em processamento para este carrinho — tente novamente em instantes\"":           true,
	"integration.Service.ConnectPagarme#0":                                                                                      true,
	"integration.Service.ConnectPagarme#1":                                                                                      true,
	"integration.Service.ConnectPagarme:\"as chaves precisam ser do mesmo ambiente (ambas de teste ou ambas de produção)\"":     true,
	"integration.Service.ConnectPagarme:\"chave pública inválida — deve começar com pk_\"":                                      true,
	"integration.Service.ConnectPagarme:\"chave pública é obrigatória\"":                                                        true,
	"integration.Service.ConnectPagarme:\"chave secreta inválida — deve começar com sk_\"":                                      true,
	"integration.Service.ConnectPagarme:\"chave secreta é obrigatória\"":                                                        true,
	"integration.Service.ConnectSmartEnvios#0":                                                                                  true,
	"integration.Service.ConnectSmartEnvios#1":                                                                                  true,
	"integration.Service.ConnectSmartEnvios:\"env must be 'sandbox' or 'production'\"":                                          true,
	"integration.Service.ConnectSmartEnvios:\"token is required\"":                                                              true,
	"integration.Service.ConnectWhatsApp:\"integração WhatsApp indisponível: credenciais Twilio não configuradas\"":             true,
	"integration.Service.Create#0":                                                                                              true,
	"integration.Service.GetCommunicationProvider:\"failed to cast to communication provider\"":                                 true,
	"integration.Service.GetCommunicationProvider:\"nenhuma integração WhatsApp configurada para esta loja\"":                   true,
	"integration.Service.GetERPProvider:\"failed to cast to ERP provider\"":                                                     true,
	"integration.Service.GetERPProvider:\"integration is not an ERP provider\"":                                                 true,
	"integration.Service.GetOAuthURL#0":                                                                                         true,
	"integration.Service.GetPagarmeWebhookStatus:\"integration is not Pagar.me\"":                                               true,
	"integration.Service.GetProviderURLs#0":                                                                                     true,
	"integration.Service.GetShippingProvider:\"no active shipping integration for this store\"":                                 true,
	"integration.Service.GetShippingProviderByName#0":                                                                           true,
	"integration.Service.GetShippingProviderByName:\"failed to cast to shipping provider\"":                                     true,
	"integration.Service.GetSocialProvider:\"failed to cast to social provider\"":                                               true,
	"integration.Service.GetSocialProvider:\"integration is not a social provider\"":                                            true,
	"integration.Service.HandleOAuthCallback#0":                                                                                 true,
	"integration.Service.ImportERPProduct:\"nenhuma das variantIds informadas existe no produto Tiny\"":                         true,
	"integration.Service.ImportERPProduct:\"product group syncer not configured\"":                                              true,
	"integration.Service.ImportERPProduct:\"product syncer not configured\"":                                                    true,
	"integration.Service.ImportERPProduct:\"produto já importado neste catálogo\"":                                              true,
	"integration.Service.ImportERPProduct:\"uma ou mais variantIds informadas não existem no produto Tiny\"":                    true,
	"integration.Service.ImportERPProduct:\"variantIds informado mas o produto não possui variações\"":                          true,
	"integration.Service.RunPagarmeWebhookLiveTest#0":                                                                           true,
	"integration.Service.RunPagarmeWebhookLiveTest:\"URL de webhook indisponível — verifique a configuração do servidor\"":      true,
	"integration.Service.RunPagarmeWebhookLiveTest:\"integration is not Pagar.me\"":                                             true,
	"integration.Service.SearchProducts:\"Produto encontrado, mas sem estoque disponível no momento\"":                          true,
	"integration.Service.SearchProducts:\"Produto não encontrado no ERP\"":                                                      true,
	"integration.Service.SendWhatsAppTemplate:\"nenhum template WhatsApp aprovado configurado para esta loja\"":                 true,
	"integration.Service.SyncProductManual:\"integração não corresponde à origem do produto\"":                                  true,
	"integration.Service.SyncProductManual:\"product syncer not configured\"":                                                   true,
	"integration.Service.SyncProductManual:\"produto não possui ID externo vinculado a um ERP\"":                                true,
	"integration.Service.TestPagarmeWebhookEndpoint:\"URL de webhook indisponível — verifique a configuração do servidor\"":     true,
	"integration.Service.TestPagarmeWebhookEndpoint:\"integração Pagar.me não encontrada\"":                                     true,
	"integration.Service.VerifyWhatsAppSender:\"integração WhatsApp indisponível: credenciais Twilio não configuradas\"":        true,
	"integration.Service.VerifyWhatsAppSender:\"registro do número ainda não foi iniciado\"":                                    true,
	"integration.Service.decryptCredentials:\"no credentials found\"":                                                           true,
	"integration.Service.getInstagramOAuthURL:\"Instagram app not configured\"":                                                 true,
	"integration.Service.getMelhorEnvioOAuthURL:\"Melhor Envio app not configured\"":                                            true,
	"integration.Service.getMercadoPagoOAuthURL:\"Mercado Pago app not configured\"":                                            true,
	"integration.Service.getTinyOAuthURL:\"Client ID não encontrado nas credenciais\"":                                          true,
	"integration.Service.getTinyOAuthURL:\"Crie primeiro o aplicativo Tiny e salve as credenciais\"":                            true,
	"integration.Service.getWhatsAppRow:\"nenhuma integração WhatsApp configurada para esta loja\"":                             true,
	"integration.Service.handleInstagramCallback:\"Instagram app not configured\"":                                              true,
	"integration.Service.handleInstagramCallback:\"OAuth state expired or invalid\"":                                            true,
	"integration.Service.handleMelhorEnvioCallback:\"Melhor Envio app not configured\"":                                         true,
	"integration.Service.handleMelhorEnvioCallback:\"OAuth state expired or invalid\"":                                          true,
	"integration.Service.handleMercadoPagoCallback:\"Mercado Pago app not configured\"":                                         true,
	"integration.Service.handleMercadoPagoCallback:\"OAuth state expired or invalid\"":                                          true,
	"integration.Service.handleTinyCallback:\"Client ID ou Client Secret não encontrado\"":                                      true,
	"integration.Service.handleTinyCallback:\"Integração Tiny não encontrada. Crie primeiro com client_id e client_secret.\"":   true,
	"integration.Service.publishInstagramPostEvent:\"failed to publish the post on Instagram\"":                                 true,
	"integration.Service.publishInstagramReelEvent:\"failed to publish the reel on Instagram\"":                                 true,
	"integration.Service.publishInstagramStoryEvent:\"failed to publish the story on Instagram\"":                               true,
	"integration.parseUUID#0":                                                                   true,
	"invitation.AcceptInvitationRequest.ToInput#0":                                              true,
	"invitation.CreateInvitationRequest.ToInput:\"invalid email format\"":                       true,
	"invitation.CreateInvitationRequest.ToInput:\"invalid inviter ID\"":                         true,
	"invitation.CreateInvitationRequest.ToInput:\"invalid role\"":                               true,
	"invitation.CreateInvitationRequest.ToInput:\"invalid store ID\"":                           true,
	"invitation.Handler.Accept:\"email not found in token claims\"":                             true,
	"invitation.Handler.List:\"invalid store ID\"":                                              true,
	"invitation.Handler.Resend:\"invalid invitation ID\"":                                       true,
	"invitation.Handler.Resend:\"invalid inviter ID\"":                                          true,
	"invitation.Handler.Resend:\"invalid store ID\"":                                            true,
	"invitation.Handler.Revoke:\"invalid invitation ID\"":                                       true,
	"invitation.Handler.Revoke:\"invalid store ID\"":                                            true,
	"invitation.Repository.Accept:\"invitation not found or already accepted\"":                 true,
	"invitation.Repository.GetByEmail:\"invitation not found\"":                                 true,
	"invitation.Repository.GetByID:\"invitation not found\"":                                    true,
	"invitation.Repository.GetByToken:\"invitation not found\"":                                 true,
	"invitation.Service.Accept#0":                                                               true,
	"invitation.Service.Accept:\"failed to verify your current store membership\"":              true,
	"invitation.Service.GetByToken#0":                                                           true,
	"invitation.parseUUID:\"invalid uuid\"":                                                     true,
	"live.Repository.DeleteEvent:\"live event not found\"":                                      true,
	"live.Repository.EndEvent:\"live event not found\"":                                         true,
	"live.Repository.EndSession:\"live session not found\"":                                     true,
	"live.Repository.GetEventByID:\"live event not found\"":                                     true,
	"live.Repository.GetEventPulse:\"event not found\"":                                         true,
	"live.Repository.GetEventWithCounts:\"event not found\"":                                    true,
	"live.Repository.GetPixDiscountPercent:\"live event not found\"":                            true,
	"live.Repository.GetSessionByID:\"live session not found\"":                                 true,
	"live.Repository.SetEventWindow:\"event not found\"":                                        true,
	"live.Repository.SetPixDiscountPercent:\"live event not found\"":                            true,
	"live.Repository.StartSession:\"live session not found\"":                                   true,
	"live.Repository.UpdateEventTitle:\"live event not found\"":                                 true,
	"live.Repository.UpdateEventUpsell:\"event upsell not found\"":                              true,
	"live.Service.ResendCheckoutMessage:\"cart not found\"":                                     true,
	"live.parseUUID:\"invalid uuid\"":                                                           true,
	"member.Repository.GetByID:\"member not found\"":                                            true,
	"member.Repository.UpdateRole:\"member not found\"":                                         true,
	"member.Service.Remove#0":                                                                   true,
	"member.Service.UpdateRole#0":                                                               true,
	"member.UpdateMemberRoleRequest.ToInput:\"invalid role\"":                                   true,
	"member.parseUUID:\"invalid uuid\"":                                                         true,
	"notification.Handler.GetSettings:\"Erro ao buscar configurações de notificação\"":          true,
	"notification.Handler.GetTestRecipient:\"Erro ao carregar destinatário de teste\"":          true,
	"notification.Handler.PreviewTemplate:\"Corpo da requisição inválido\"":                     true,
	"notification.Handler.PreviewTemplate:\"Template não pode estar vazio\"":                    true,
	"notification.Handler.SendTest:\"Configure o destinatário de teste antes de enviar.\"":      true,
	"notification.Handler.SendTest:\"Corpo da requisição inválido\"":                            true,
	"notification.Handler.SendTest:\"Não foi possível enviar a notificação de teste\"":          true,
	"notification.Handler.SendTest:\"Type e template são obrigatórios\"":                        true,
	"notification.Handler.SendTestEmail:\"Assunto ou corpo precisam estar preenchidos\"":        true,
	"notification.Handler.SendTestEmail:\"Corpo da requisição inválido\"":                       true,
	"notification.Handler.SendTestEmail:\"Email destinatário é obrigatório\"":                   true,
	"notification.Handler.SendTestEmail:\"Não foi possível enviar o email de teste\"":           true,
	"notification.Handler.StartTestSetup:\"Erro ao iniciar configuração do destinatário\"":      true,
	"notification.Handler.UpdateSettings#0":                                                     true,
	"notification.Handler.UpdateSettings:\"Corpo da requisição inválido\"":                      true,
	"notification.Handler.UpdateSettings:\"Erro ao atualizar configurações de notificação\"":    true,
	"order.Service.GetByID#0":                                                                   true,
	"order.Service.GetByID#1":                                                                   true,
	"order.Service.GetUpsellSummary#0":                                                          true,
	"order.Service.RegenerateCheckout#0":                                                        true,
	"order.Service.Update#0":                                                                    true,
	"order.Service.Update#1":                                                                    true,
	"order.Service.UpdateShippingAddress#0":                                                     true,
	"payment.Service.GetProvider:\"failed to cast to payment provider\"":                        true,
	"payment.Service.GetProvider:\"integration is not a payment provider\"":                     true,
	"postcheckout.Handler.ConfirmDelivery:\"Pedido não encontrado\"":                            true,
	"postcheckout.Handler.GetOrder:\"Erro ao carregar pedido\"":                                 true,
	"postcheckout.Handler.GetOrder:\"Pedido não encontrado\"":                                   true,
	"postcheckout.Handler.MarkDelivered:\"Loja não identificada\"":                              true,
	"postcheckout.Handler.MarkDelivered:\"Pedido não encontrado\"":                              true,
	"product.CreateProductRequest.ToInput#0":                                                    true,
	"product.CreateProductRequest.ToInput:\"invalid external source\"":                          true,
	"product.CreateProductRequest.ToInput:\"invalid groupId\"":                                  true,
	"product.CreateProductRequest.ToInput:\"invalid price\"":                                    true,
	"product.CreateProductRequest.ToInput:\"invalid store ID\"":                                 true,
	"product.Handler.AddImage:\"invalid product ID\"":                                           true,
	"product.Handler.AddImage:\"invalid store ID\"":                                             true,
	"product.Handler.Delete:\"invalid product ID\"":                                             true,
	"product.Handler.Delete:\"invalid store ID\"":                                               true,
	"product.Handler.DeleteImage:\"invalid image ID\"":                                          true,
	"product.Handler.DeleteImage:\"invalid product ID\"":                                        true,
	"product.Handler.DeleteImage:\"invalid store ID\"":                                          true,
	"product.Handler.GetByID:\"invalid product ID\"":                                            true,
	"product.Handler.GetByID:\"invalid store ID\"":                                              true,
	"product.Handler.GetStats:\"invalid store ID\"":                                             true,
	"product.Handler.List:\"invalid store ID\"":                                                 true,
	"product.Repository.Delete:\"product not found\"":                                           true,
	"product.Repository.GetByID:\"product not found\"":                                          true,
	"product.Repository.GetByKeyword:\"product not found\"":                                     true,
	"product.Repository.Update:\"product not found\"":                                           true,
	"product.Service.Create:\"keyword already in use\"":                                         true,
	"product.Service.Update#0":                                                                  true,
	"product.Service.resolveKeyword#0":                                                          true,
	"product.Service.resolveKeyword:\"keyword range exhausted (max 9999)\"":                     true,
	"product.UpdateProductRequest.ToInput#0":                                                    true,
	"product.UpdateProductRequest.ToInput:\"invalid price\"":                                    true,
	"product.UpdateProductRequest.ToInput:\"invalid product ID\"":                               true,
	"product.UpdateProductRequest.ToInput:\"invalid store ID\"":                                 true,
	"productgroup.CreateGroupRequest.ToInput:\"invalid external source\"":                       true,
	"productgroup.CreateGroupRequest.ToInput:\"invalid store ID\"":                              true,
	"productgroup.Handler.AddImage:\"invalid group ID\"":                                        true,
	"productgroup.Handler.AddImage:\"invalid store ID\"":                                        true,
	"productgroup.Handler.Delete:\"invalid group ID\"":                                          true,
	"productgroup.Handler.Delete:\"invalid store ID\"":                                          true,
	"productgroup.Handler.DeleteImage:\"invalid group ID\"":                                     true,
	"productgroup.Handler.DeleteImage:\"invalid image ID\"":                                     true,
	"productgroup.Handler.DeleteImage:\"invalid store ID\"":                                     true,
	"productgroup.Handler.GetByID:\"invalid group ID\"":                                         true,
	"productgroup.Handler.GetByID:\"invalid store ID\"":                                         true,
	"productgroup.Handler.List:\"invalid store ID\"":                                            true,
	"productgroup.Handler.Update:\"invalid group ID\"":                                          true,
	"productgroup.Handler.Update:\"invalid store ID\"":                                          true,
	"productgroup.Repository.GetByID:\"product group not found\"":                               true,
	"productgroup.Repository.Update:\"product group not found\"":                                true,
	"productgroup.Service.Create#0":                                                             true,
	"productgroup.Service.Create#1":                                                             true,
	"productgroup.Service.Create#2":                                                             true,
	"productgroup.Service.Create#3":                                                             true,
	"productgroup.Service.Create#4":                                                             true,
	"productgroup.Service.Create#5":                                                             true,
	"productgroup.Service.Create#6":                                                             true,
	"productgroup.Service.Create:\"keyword range exhausted (max 9999)\"":                        true,
	"productgroup.Service.Update#0":                                                             true,
	"productgroup.SyncerAdapter.ImportFromERP:\"grupo de produto já importado neste catálogo\"": true,
	"store.Handler.UpdateByID:\"no store access\"":                                              true,
	"store.Handler.UpdateCartSettingsByID:\"no store access\"":                                  true,
	"store.Handler.UpdateShippingDefaultsByID:\"no store access\"":                              true,
	"store.Handler.UploadLogo:\"failed to generate logo URL\"":                                  true,
	"store.Handler.UploadLogo:\"failed to read file\"":                                          true,
	"store.Handler.UploadLogo:\"failed to upload file\"":                                        true,
	"store.Handler.UploadLogo:\"file is required\"":                                             true,
	"store.Handler.UploadLogo:\"file too large, maximum size is 2MB\"":                          true,
	"store.Handler.UploadLogo:\"invalid file type, accepted: JPG, PNG, GIF, WebP\"":             true,
	"store.Handler.UploadLogo:\"storage not configured\"":                                       true,
	"store.Repository.GetByID:\"store not found\"":                                              true,
	"store.Repository.GetBySlug:\"store not found\"":                                            true,
	"store.Repository.GetByUserID:\"store not found\"":                                          true,
	"store.Repository.Update:\"store not found\"":                                               true,
	"store.Repository.UpdateCartSettings:\"store not found\"":                                   true,
	"store.Repository.UpdateLogoURL:\"store not found\"":                                        true,
	"store.Repository.UpdateShippingDefaults:\"store not found\"":                               true,
	"store.Service.GetByClerkUserID:\"user not found\"":                                         true,
	"store.Service.Update#0":                                                                    true,
	"store.parseUUID:\"invalid uuid\"":                                                          true,
	"user.Repository.GetUserByClerkID:\"user not found\"":                                       true,
	"user.Repository.GetUserByEmail:\"user not found\"":                                         true,
	"user.Repository.GetUserByID:\"user not found\"":                                            true,
	"user.Repository.UpdateUser:\"user not found\"":                                             true,
	"user.parseUUID:\"invalid uuid\"":                                                           true,

	// Trazidos pelo épico Evento Guarda-Chuva, que entrou depois de este ratchet
	// congelar a baseline na branch onde o sistema de erros nasceu. São
	// not-found, validação de entrada e config — a categoria que o próprio teste
	// manda registrar aqui em vez de tipar às pressas. A maioria é da publicação
	// agendada, que é uma fatia inteira nova; tipá-los pertence à iniciativa de
	// erros, não a esta entrega.
	"integration.Handler.ListInstagramScheduledPublications:\"invalid status filter\"":                   true,
	"integration.Handler.ScheduleInstagramPublish:\"failed to read file\"":                               true,
	"integration.Handler.ScheduleInstagramPublish:\"failed to upload the media\"":                        true,
	"integration.Handler.ScheduleInstagramPublish:\"file is required\"":                                  true,
	"integration.Handler.ScheduleInstagramPublish:\"storage not configured\"":                            true,
	"integration.Service.CancelPublish#0":                                                                true,
	"integration.Service.CancelPublish:\"scheduled publication not found\"":                              true,
	"integration.Service.SchedulePublish#0":                                                              true,
	"integration.Service.SchedulePublish:\"asset is required\"":                                          true,
	"integration.Service.SchedulePublish:\"endsAt must be after startsAt\"":                              true,
	"integration.Service.SchedulePublish:\"endsAt must be after the scheduled publication\"":             true,
	"integration.Service.SchedulePublish:\"mediaKind must be post, reel or story\"":                      true,
	"integration.Service.SchedulePublish:\"scheduledFor is too far ahead (max 60 days)\"":                true,
	"integration.Service.SchedulePublish:\"select at least one product for the promotion\"":              true,
	"integration.parseSchedulePublishForm#0":                                                             true,
	"integration.parseSchedulePublishForm#1":                                                             true,
	"integration.parseSchedulePublishForm:\"invalid productIds\"":                                        true,
	"integration.validateInstagramAsset:\"file too large, maximum size is 8MB\"":                         true,
	"integration.validateInstagramAsset:\"image too large, maximum size is 8MB\"":                        true,
	"integration.validateInstagramAsset:\"invalid file type — Instagram Reels require an MP4 video\"":    true,
	"integration.validateInstagramAsset:\"invalid file type — Instagram requires a JPEG image\"":         true,
	"integration.validateInstagramAsset:\"invalid file type — send a JPEG photo or an MP4 video\"":       true,
	"integration.validateInstagramAsset:\"mediaKind must be post, reel or story\"":                       true,
	"integration.validateInstagramAsset:\"video too large, maximum size is 100MB\"":                      true,
	"integration.validateInstagramAsset:\"video too large, maximum size is 300MB\"":                      true,
	"live.Repository.GetSessionLiveModeState:\"session not found\"":                                      true,
	"live.Repository.SetMedia:\"media id is required\"":                                                  true,
	"live.Repository.SetSessionActiveProduct:\"session not found\"":                                      true,
	"live.Repository.SetSessionProcessingPaused:\"session not found\"":                                   true,
	"live.Repository.SetWaitlistNotifiedTTLMinutes:\"live event not found\"":                             true,
	"live.Repository.UpsertSessionProduct:\"session product not found\"":                                 true,
	"live.Service.Create:\"endsAt precisa ser depois de startsAt\"":                                      true,
	"live.Service.Create:\"endsAt é obrigatório: o evento precisa de uma data de encerramento\"":         true,
	"live.Service.ResendCheckoutMessage#0":                                                               true,
	"live.Service.Start:\"event not found\"":                                                             true,
	"live.Service.Update:\"endsAt não pode ser removido: o evento precisa de uma data de encerramento\"": true,
	"live.Service.Update:\"endsAt precisa ser depois de startsAt\"":                                      true,
	"live.Service.resolveSessionOfEvent:\"session not found\"":                                           true,
	"notification.Handler.ListUndelivered:\"Erro ao listar mensagens não entregues\"":                    true,
	"notification.Handler.ListUndelivered:\"eventId é obrigatório\"":                                     true,
}
