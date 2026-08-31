package providers

import (
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/lib/ratelimit"
)

func factoryComBling(t *testing.T) (*Factory, *ratelimit.Manager) {
	t.Helper()
	mgr := ratelimit.NewManager(zap.NewNop())
	f := NewFactory(FactoryConfig{
		Logger:            zap.NewNop(),
		RateLimitManager:  mgr,
		BlingClientID:     "app-id",
		BlingClientSecret: "app-secret",
		BlingConstructor: func(cfg BlingConfig) (ERPProvider, error) {
			return &blingFalso{cfg: cfg}, nil
		},
	})
	return f, mgr
}

type blingFalso struct {
	ERPProvider
	cfg BlingConfig
}

// O teto do Bling é POR CONTA somando todos os apps do lojista. Duas lojas
// LiveCart ligadas à MESMA empresa Bling têm de dividir UM balde — chavear por
// integração daria dois baldes para uma cota só, ou seja o DOBRO do teto, e a
// descoberta viria como 429 no meio da venda.
func TestFactoryChaveiaOBaldeDoBlingPelaConta(t *testing.T) {
	f, _ := factoryComBling(t)

	const conta = "9db3b9e60022d0eddb121a4319dfbe15"
	novo := func(integrationID string) ratelimit.RateLimiter {
		p, err := f.CreateERPProvider(ProviderConfig{
			IntegrationID: integrationID,
			StoreID:       "loja-" + integrationID,
			Type:          ProviderTypeERP,
			Name:          ProviderBling,
			Credentials:   &Credentials{AccessToken: "at"},
			Metadata:      map[string]any{MetadataBlingCompanyID: conta},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p.(*blingFalso).cfg.RateLimiter
	}

	a, b := novo("int-A"), novo("int-B")
	if a == nil || b == nil {
		t.Fatal("o provider saiu sem limitador")
	}
	if a != b {
		t.Error("duas integrações na MESMA conta Bling receberam baldes DIFERENTES — " +
			"isso é o dobro do teto de 3 req/s da conta, e o 429 aparece no meio da live")
	}

	// Conta diferente → balde diferente, senão uma loja estrangula a outra.
	p, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-C", StoreID: "loja-C",
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{AccessToken: "at"},
		Metadata:    map[string]any{MetadataBlingCompanyID: "outra-conta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.(*blingFalso).cfg.RateLimiter == a {
		t.Error("contas diferentes compartilharam balde — uma loja estrangularia a outra")
	}
}

// Antes da primeira leitura de identidade não há conta conhecida. O provider
// não pode sair SEM freio — um balde a mais é melhor do que balde nenhum.
func TestFactoryBlingSemContaAindaTemFreio(t *testing.T) {
	f, _ := factoryComBling(t)

	p, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-nova", StoreID: "loja-1",
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{AccessToken: "at"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.(*blingFalso).cfg.RateLimiter == nil {
		t.Fatal("sem conta conhecida o provider saiu SEM limitador")
	}
}

// O limitador do Bling tem de ser PREDITIVO. O AdaptiveLimiter só ganha estado
// em UpdateFromHeaders, e a API do Bling não manda header de cota nenhum
// (medido) — ele devolveria "pode passar" para sempre.
func TestFactoryBlingUsaLimitadorPreditivoENaoOAdaptativo(t *testing.T) {
	f, _ := factoryComBling(t)

	p, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-1", StoreID: "loja-1",
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{AccessToken: "at"},
		Metadata:    map[string]any{MetadataBlingCompanyID: "conta-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ehFixo := p.(*blingFalso).cfg.RateLimiter.(*ratelimit.Fixo); !ehFixo {
		t.Errorf("o Bling recebeu %T; sem header de cota, só o Fixo freia de verdade",
			p.(*blingFalso).cfg.RateLimiter)
	}
}

// O Tiny NÃO pode mudar de limitador: ele recebe headers e o adaptativo é o
// certo para ele. Consertar o Bling não pode mexer em quem fatura hoje.
func TestFactoryTinyContinuaNoAdaptativo(t *testing.T) {
	mgr := ratelimit.NewManager(zap.NewNop())
	f := NewFactory(FactoryConfig{
		Logger:           zap.NewNop(),
		RateLimitManager: mgr,
		TinyConstructor: func(cfg TinyConfig) (ERPProvider, error) {
			return &tinyFalso{cfg: cfg}, nil
		},
	})

	p, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-tiny", StoreID: "loja-1",
		Type: ProviderTypeERP, Name: ProviderTiny,
		Credentials: &Credentials{AccessToken: "at", Extra: map[string]any{
			"client_id": "a", "client_secret": "b",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ehAdaptativo := p.(*tinyFalso).cfg.RateLimiter.(*ratelimit.AdaptiveLimiter); !ehAdaptativo {
		t.Errorf("o Tiny recebeu %T, queria o AdaptiveLimiter — ele tem headers para reconciliar",
			p.(*tinyFalso).cfg.RateLimiter)
	}
}

type tinyFalso struct {
	ERPProvider
	cfg TinyConfig
}

// As credenciais do aplicativo vêm do ambiente (app único do LiveCart), com
// escape para o lojista que preferir o próprio aplicativo privado.
func TestFactoryBlingUsaCredencialDoAppComEscapeParaAPrivada(t *testing.T) {
	f, _ := factoryComBling(t)

	padrao, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-1", StoreID: "loja-1",
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{AccessToken: "at"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := padrao.(*blingFalso).cfg; c.ClientID != "app-id" || c.ClientSecret != "app-secret" {
		t.Errorf("sem credencial própria devia usar a do ambiente, veio %q/%q", c.ClientID, c.ClientSecret)
	}

	proprio, err := f.CreateERPProvider(ProviderConfig{
		IntegrationID: "int-2", StoreID: "loja-2",
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{AccessToken: "at", Extra: map[string]any{
			"client_id": "do-lojista", "client_secret": "secret-do-lojista",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := proprio.(*blingFalso).cfg; c.ClientID != "do-lojista" {
		t.Errorf("o aplicativo privado do lojista devia vencer, veio %q", c.ClientID)
	}
}

// Sem construtor injetado o factory tem de RECUSAR, não devolver nil.
func TestFactoryBlingSemConstrutorRecusa(t *testing.T) {
	f := NewFactory(FactoryConfig{Logger: zap.NewNop()})
	if _, err := f.CreateERPProvider(ProviderConfig{
		Type: ProviderTypeERP, Name: ProviderBling,
		Credentials: &Credentials{},
	}); err == nil {
		t.Error("queria erro explícito quando o construtor do Bling não foi ligado no boot")
	}
}
