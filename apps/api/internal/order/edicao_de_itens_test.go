package order_test

// A porta que o painel abre para editar itens de um pedido não pago.
//
// A mecânica de mutação é do checkout e tem testes lá. O que este arquivo trava é
// o que o domínio order acrescenta — e o principal é a FRONTEIRA DE POSSE.
//
// O checkout carrega carrinho por TOKEN, e token não prova nada sobre a loja: ele
// prova o carrinho. Quem prova a loja é a consulta escopada por store_id que roda
// aqui antes. Se o token escapar para uma loja que não é a dona, a lojista A
// edita o pedido da lojista B — mexendo em estoque e reserva de ERP alheios.
//
// Invariantes:
//   P1 pedido de OUTRA loja: 404 e o token NUNCA chega ao checkout
//   P2 checkout não ligado no boot: 422 com código, não panic de nil
//   P3 caminho normal: o checkout recebe o token DAQUELE carrinho
//   P4 pedido sem token de checkout: recusa em vez de chamar com string vazia

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/httpx"
)

// ─── editor falso ───────────────────────────────────────────────────────────

type chamadaDeEdicao struct {
	op        string
	token     string
	productID string
	itemID    string
	quantity  int
}

type editorFalso struct {
	chamadas []chamadaDeEdicao
	erro     error
}

func (e *editorFalso) AddCartItemAsMerchant(_ context.Context, token, productID string, quantity int) error {
	e.chamadas = append(e.chamadas, chamadaDeEdicao{op: "add", token: token, productID: productID, quantity: quantity})
	return e.erro
}

func (e *editorFalso) SetCartItemQuantityAsMerchant(_ context.Context, token, itemID string, quantity int) error {
	e.chamadas = append(e.chamadas, chamadaDeEdicao{op: "set", token: token, itemID: itemID, quantity: quantity})
	return e.erro
}

func (e *editorFalso) RemoveCartItemAsMerchant(_ context.Context, token, itemID string) error {
	e.chamadas = append(e.chamadas, chamadaDeEdicao{op: "remove", token: token, itemID: itemID})
	return e.erro
}

func servicoComEditor(t *testing.T, editor order.CartItemEditor) *order.Service {
	t.Helper()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())
	if editor != nil {
		svc.SetCartItemEditor(editor)
	}
	return svc
}

// ─── P1 ─────────────────────────────────────────────────────────────────────

func TestEdicaoDeItens_NaoAtravessaLojas(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	_, eventoDaOutra := seedIsolatedStore(t, "edit1b")
	prod := seedProduct(t, mustStoreOf(t, eventoDaOutra), 5000)
	cartDaOutra := insertCart(t, eventoDaOutra, "@outra", "tok-edit1b", 91001, "pending", nil)
	addItem(t, cartDaOutra, prod, 1, 5000)

	minhaLoja, _ := seedIsolatedStore(t, "edit1a")

	editor := &editorFalso{}
	svc := servicoComEditor(t, editor)

	// A loja A tentando editar o carrinho da loja B, com um product id válido.
	err := svc.AddItem(ctx, order.AddOrderItemInput{
		OrderID:   cartDaOutra,
		StoreID:   minhaLoja,
		ProductID: prod,
		Quantity:  1,
	})
	if err == nil {
		t.Fatal("P1: a loja A editou o pedido da loja B")
	}
	if len(editor.chamadas) != 0 {
		t.Errorf("P1: o token do carrinho alheio chegou ao checkout: %+v", editor.chamadas)
	}

	// 404, não 403: revelar "existe mas não é seu" já entrega que o pedido existe.
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 404 {
		t.Errorf("P1: erro = %v; esperava 404", err)
	}

	// As outras duas portas têm de fechar do mesmo jeito.
	if err := svc.SetItemQuantity(ctx, order.SetOrderItemQuantityInput{
		OrderID: cartDaOutra, StoreID: minhaLoja, ItemID: "qualquer", Quantity: 2,
	}); err == nil {
		t.Error("P1: PATCH de quantidade atravessou lojas")
	}
	if err := svc.RemoveItem(ctx, order.RemoveOrderItemInput{
		OrderID: cartDaOutra, StoreID: minhaLoja, ItemID: "qualquer",
	}); err == nil {
		t.Error("P1: DELETE de item atravessou lojas")
	}
	if len(editor.chamadas) != 0 {
		t.Errorf("P1: alguma porta deixou o token alheio passar: %+v", editor.chamadas)
	}
}

// ─── P2 ─────────────────────────────────────────────────────────────────────

// O checkout só é construído quando a integração existe (main.go). Sem ele, a
// edição tem de responder com código próprio — não estourar num nil.
func TestEdicaoDeItens_SemCheckoutLigadoRecusaComCodigo(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID := seedIsolatedStore(t, "edit2")
	prod := seedProduct(t, storeID, 5000)
	cartID := insertCart(t, eventID, "@sem", "tok-edit2", 91002, "pending", nil)
	addItem(t, cartID, prod, 1, 5000)

	svc := servicoComEditor(t, nil)

	err := svc.AddItem(ctx, order.AddOrderItemInput{
		OrderID: cartID, StoreID: storeID, ProductID: prod, Quantity: 1,
	})
	if err == nil {
		t.Fatal("P2: aceitou editar sem o checkout ligado")
	}
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) {
		t.Fatalf("P2: erro sem tipo (%T)", err)
	}
	if svcErr.Reason != string(httpx.CodeCartEditUnavailable) {
		t.Errorf("P2: reason = %q, esperava %q", svcErr.Reason, httpx.CodeCartEditUnavailable)
	}
}

// ─── P3 ─────────────────────────────────────────────────────────────────────

func TestEdicaoDeItens_DelegaComOTokenDoCarrinhoCerto(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID := seedIsolatedStore(t, "edit3")
	prod := seedProduct(t, storeID, 8990)
	const token = "tok-edit3"
	cartID := insertCart(t, eventID, "@cliente", token, 91003, "pending", nil)
	addItem(t, cartID, prod, 2, 8990)

	editor := &editorFalso{}
	svc := servicoComEditor(t, editor)

	if err := svc.AddItem(ctx, order.AddOrderItemInput{
		OrderID: cartID, StoreID: storeID, ProductID: prod, Quantity: 3,
	}); err != nil {
		t.Fatalf("P3 add: %v", err)
	}
	if err := svc.SetItemQuantity(ctx, order.SetOrderItemQuantityInput{
		OrderID: cartID, StoreID: storeID, ItemID: "item-1", Quantity: 5,
	}); err != nil {
		t.Fatalf("P3 set: %v", err)
	}
	if err := svc.RemoveItem(ctx, order.RemoveOrderItemInput{
		OrderID: cartID, StoreID: storeID, ItemID: "item-1",
	}); err != nil {
		t.Fatalf("P3 remove: %v", err)
	}

	if len(editor.chamadas) != 3 {
		t.Fatalf("P3: %d chamada(s) ao checkout, esperava 3: %+v", len(editor.chamadas), editor.chamadas)
	}
	for _, c := range editor.chamadas {
		if c.token != token {
			t.Errorf("P3: operação %q foi com token %q, esperava %q — token errado edita "+
				"o carrinho errado", c.op, c.token, token)
		}
	}
	if got := editor.chamadas[0]; got.productID != prod || got.quantity != 3 {
		t.Errorf("P3: add chegou como %+v", got)
	}
	if got := editor.chamadas[1]; got.itemID != "item-1" || got.quantity != 5 {
		t.Errorf("P3: set chegou como %+v", got)
	}
	if got := editor.chamadas[2]; got.itemID != "item-1" {
		t.Errorf("P3: remove chegou como %+v", got)
	}
}

// O erro do checkout (pago, 409 da trava otimista, estoque insuficiente) sobe
// intacto: é ele que carrega a mensagem que o lojista precisa ler.
func TestEdicaoDeItens_ErroDoCheckoutSobeIntacto(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID := seedIsolatedStore(t, "edit4")
	prod := seedProduct(t, storeID, 1000)
	cartID := insertCart(t, eventID, "@cliente", "tok-edit4", 91004, "pending", nil)
	addItem(t, cartID, prod, 1, 1000)

	recusa := httpx.ErrConflict("carrinho já foi pago")
	editor := &editorFalso{erro: recusa}
	svc := servicoComEditor(t, editor)

	err := svc.AddItem(ctx, order.AddOrderItemInput{
		OrderID: cartID, StoreID: storeID, ProductID: prod, Quantity: 1,
	})
	if !errors.Is(err, recusa) {
		t.Errorf("erro do checkout foi trocado no caminho: %v", err)
	}
}

// ─── P4 ─────────────────────────────────────────────────────────────────────

func TestEdicaoDeItens_PedidoSemTokenRecusa(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID, eventID := seedIsolatedStore(t, "edit5")
	prod := seedProduct(t, storeID, 1000)
	cartID := insertCart(t, eventID, "@semtoken", "tok-edit5", 91005, "pending", nil)
	addItem(t, cartID, prod, 1, 1000)

	if _, err := testPool.Exec(ctx, `UPDATE carts SET token = '' WHERE id = $1`, cartID); err != nil {
		t.Fatalf("limpando o token: %v", err)
	}

	editor := &editorFalso{}
	svc := servicoComEditor(t, editor)

	if err := svc.AddItem(ctx, order.AddOrderItemInput{
		OrderID: cartID, StoreID: storeID, ProductID: prod, Quantity: 1,
	}); err == nil {
		t.Error("P4: aceitou editar pedido sem token de checkout")
	}
	if len(editor.chamadas) != 0 {
		t.Errorf("P4: chamou o checkout com token vazio: %+v", editor.chamadas)
	}
}

// mustStoreOf devolve a loja de um evento — os helpers de seed criam os dois
// juntos, mas os testes acima às vezes só guardam um dos ids.
func mustStoreOf(t *testing.T, eventID string) string {
	t.Helper()
	var storeID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT store_id::text FROM live_events WHERE id = $1`, eventID,
	).Scan(&storeID); err != nil {
		t.Fatalf("store do evento: %v", err)
	}
	return storeID
}
