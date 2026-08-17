package product

// Buscar produto no catálogo pelo código que o lojista tem à mão.
//
// A busca interna procurava só `name` e `keyword`. Com a peça na frente, o que
// ele tem é o SKU da etiqueta ou o código de barras — procurar pelo nome exige
// lembrar como o produto foi cadastrado, e ele foi cadastrado pelo ERP.
//
// Dois defeitos sustentavam o sintoma, e o primeiro é o que fazia "adicionar a
// coluna na busca" não bastar:
//
//   S1 o SKU não sobrevivia. `ShippingProfile` guarda o SKU, mas o perfil que
//      vem do ERP (`ERPShippingProfile`) só tem peso e medidas — sincronizar
//      substituía o perfil inteiro e zerava o identificador. Produto importado
//      com SKU perdia o SKU no primeiro webhook de estoque.
//   S2 o código de barras não existia. O Tiny devolve `gtin` e o parser já o
//      lia; não havia coluna para gravá-lo.

import (
	"testing"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product/domain"
)

// ─── S1 ─────────────────────────────────────────────────────────────────────

// perfilLocal simula o que já está gravado no produto.
func perfilLocal(sku, barcode string) domain.ShippingProfile {
	peso, alt, larg, comp := 500, 10, 20, 30
	return domain.ShippingProfile{
		WeightGrams: &peso, HeightCm: &alt, WidthCm: &larg, LengthCm: &comp,
		SKU: sku, Barcode: barcode, PackageFormat: domain.PackageFormatBox,
	}
}

// perfilDoERP é o que `erpShippingToDomain` produz: medidas, e NENHUM
// identificador — é a forma real de ERPShippingProfile.
func perfilDoERP() domain.ShippingProfile {
	return erpShippingToDomain(&providers.ERPShippingProfile{
		WeightGrams: 600, HeightCm: 12, WidthCm: 22, LengthCm: 32, PackageFormat: "box",
	})
}

func TestSincronizarNaoApagaOsIdentificadores(t *testing.T) {
	if perfilDoERP().SKU != "" {
		t.Fatal("o perfil vindo do ERP passou a trazer SKU; este teste existe " +
			"porque ele NÃO traz, e substituí-lo cru apagava o identificador")
	}

	atual := perfilLocal("PA440450000093", "7891234567895")
	doERP := perfilDoERP()

	// A regra do sync: as medidas vêm do ERP, os identificadores ficam.
	resultado := mesclarIdentificadores(atual, doERP, "", "")

	if resultado.SKU != "PA440450000093" {
		t.Errorf("SKU virou %q depois de sincronizar — era o que sumia a cada "+
			"webhook de estoque e deixava a busca por SKU sem achar nada",
			resultado.SKU)
	}
	if resultado.Barcode != "7891234567895" {
		t.Errorf("código de barras virou %q depois de sincronizar", resultado.Barcode)
	}
	if resultado.WeightGrams == nil || *resultado.WeightGrams != 600 {
		t.Error("as medidas do ERP não foram aplicadas; preservar identificador " +
			"não pode congelar o resto do perfil")
	}
}

// Quando o ERP INFORMA o identificador, ele manda: é a fonte da verdade do
// catálogo, e é isso que faz o "Sincronizar com o ERP" preencher quem está
// vazio hoje.
func TestSincronizarAplicaOIdentificadorDoERP(t *testing.T) {
	atual := perfilLocal("", "")
	resultado := mesclarIdentificadores(atual, perfilDoERP(), "SKU-NOVO", "7890000000001")

	if resultado.SKU != "SKU-NOVO" {
		t.Errorf("SKU do ERP não foi aplicado: %q", resultado.SKU)
	}
	if resultado.Barcode != "7890000000001" {
		t.Errorf("código de barras do ERP não foi aplicado: %q", resultado.Barcode)
	}
}

// O lojista pode ter preenchido o SKU à mão num produto que o ERP não conhece
// pelo mesmo código. ERP vazio não apaga o que ele digitou.
func TestERPSemIdentificadorNaoSobrepoeOLocal(t *testing.T) {
	atual := perfilLocal("DIGITADO-A-MAO", "7899999999999")
	resultado := mesclarIdentificadores(atual, perfilDoERP(), "", "")

	if resultado.SKU != "DIGITADO-A-MAO" {
		t.Errorf("SKU local foi sobreposto por vazio: %q", resultado.SKU)
	}
	if resultado.Barcode != "7899999999999" {
		t.Errorf("código de barras local foi sobreposto por vazio: %q", resultado.Barcode)
	}
}

// ─── S2 ─────────────────────────────────────────────────────────────────────

// O identificador atravessa DTO→domínio→DTO sem se perder. É o caminho da
// variação, onde o valor passa pelo DTO antes de ser gravado — e onde o SKU
// ficava pelo caminho porque a conversão não o carregava.
func TestIdentificadoresSobrevivemAoRoundTripDoDTO(t *testing.T) {
	original := perfilLocal("SKU-VAR-1", "7891111111118")

	dto := shippingDomainToDTO(original)
	if dto.SKU != "SKU-VAR-1" || dto.Barcode != "7891111111118" {
		t.Fatalf("ida para DTO perdeu identificador: sku=%q barcode=%q", dto.SKU, dto.Barcode)
	}

	volta, err := shippingDTOToDomain(dto)
	if err != nil {
		t.Fatalf("volta do DTO: %v", err)
	}
	if volta.SKU != "SKU-VAR-1" {
		t.Errorf("SKU sumiu na volta: %q", volta.SKU)
	}
	if volta.Barcode != "7891111111118" {
		t.Errorf("código de barras sumiu na volta: %q", volta.Barcode)
	}
}

// A importação monta o perfil com os identificadores do produto do ERP — é o
// que faz produto novo já nascer buscável pelo código.
func TestImportacaoLevaOsIdentificadoresDoERP(t *testing.T) {
	perfil := erpShippingWithIDs(providers.ERPProduct{
		ID:   "847615890",
		SKU:  "PA440450000093",
		GTIN: "7891234567895",
		Shipping: &providers.ERPShippingProfile{
			WeightGrams: 250, HeightCm: 5, WidthCm: 10, LengthCm: 15, PackageFormat: "box",
		},
	})

	if perfil.SKU != "PA440450000093" {
		t.Errorf("importação não levou o SKU: %q", perfil.SKU)
	}
	if perfil.Barcode != "7891234567895" {
		t.Errorf("importação não levou o código de barras: %q", perfil.Barcode)
	}
}

// Produto do ERP SEM perfil de frete ainda tem identificador. Amarrar um ao
// outro deixaria sem SKU justamente o produto que o lojista não cadastrou
// dimensões — que é o caso comum de quem só quer vender na live.
func TestProdutoSemDimensoesAindaLevaIdentificador(t *testing.T) {
	perfil := erpShippingWithIDs(providers.ERPProduct{
		ID: "1", SKU: "SO-SKU", GTIN: "7890000000002", Shipping: nil,
	})

	if perfil.SKU != "SO-SKU" || perfil.Barcode != "7890000000002" {
		t.Errorf("produto sem dimensões ficou sem identificador: sku=%q barcode=%q",
			perfil.SKU, perfil.Barcode)
	}
}
