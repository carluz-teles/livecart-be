package integration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Fluxo OAuth do Bling.
//
// Três diferenças em relação ao Tiny que NÃO são detalhe:
//
//  1. O LiveCart tem UM aplicativo no Bling (credenciais em env), então o
//     lojista conecta em UM CLIQUE — não precisa criar app nem colar
//     client_id/secret. O escape para quem preferir o próprio aplicativo
//     privado existe no factory, via Credentials.Extra.
//  2. O `code` vive UM MINUTO e reusá-lo REVOGA o acesso do usuário (a doc é
//     literal). Por isso o state é consumido ATOMICAMENTE e a troca não tem
//     retry.
//  3. Ao final gravamos a identidade da CONTA (erp_account_id). Ela é a chave
//     de cota — o teto do Bling é por conta — e é por ela que o webhook de URL
//     única resolve a loja.

// getBlingOAuthURL monta a URL de autorização e guarda o state.
func (s *Service) getBlingOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	clientID := config.BlingClientID.String()
	if clientID == "" {
		return nil, httpx.ErrUnprocessable("Aplicativo Bling não configurado no servidor")
	}

	ctx := logger.WithStore(context.Background(), storeID, "")

	// A regra de negócio antes de mandar o lojista para a tela do Bling: uma
	// loja integra UM ERP. Descobrir isso DEPOIS de autorizar seria fazer o
	// lojista percorrer a tela de consentimento para nada — e ainda gastaria uma
	// das vagas do teto de usuários do aplicativo não homologado.
	if existente, err := s.repo.GetActiveERP(ctx, storeID); err == nil && existente != nil &&
		existente.Provider != string(providers.ProviderBling) {
		return nil, httpx.ErrUnprocessable(
			"Esta loja já usa o " + nomeAmigavelDoERP(existente.Provider) +
				". Só é possível manter um ERP conectado por vez — desconecte-o antes de conectar o Bling.")
	}

	state := uuid.New().String()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, string(providers.ProviderBling), ""); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	return &GetOAuthURLOutput{
		AuthURL: providererp.BlingAuthorizeURL(clientID, state),
		State:   state,
	}, nil
}

func nomeAmigavelDoERP(provider string) string {
	switch provider {
	case string(providers.ProviderTiny):
		return "Tiny"
	case string(providers.ProviderBling):
		return "Bling"
	default:
		return provider
	}
}

// handleBlingCallback troca o code por tokens e conclui a conexão.
func (s *Service) handleBlingCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	clientID := config.BlingClientID.String()
	clientSecret := config.BlingClientSecret.String()
	if clientID == "" || clientSecret == "" {
		return nil, httpx.ErrUnprocessable("Aplicativo Bling não configurado no servidor")
	}

	// Consumo ATÔMICO: valida e apaga numa query só.
	//
	// Com o par GetOAuthState + defer Delete haveria uma janela em que dois
	// callbacks simultâneos (duplo clique, retry do navegador) passariam os dois
	// pela validação e tentariam trocar o MESMO code. A doc do Bling avisa que
	// reusar um code válido REVOGA o acesso do usuário — o custo de errar aqui
	// não é um erro na tela, é a loja desconectada.
	estado, err := s.repo.ConsumeOAuthState(ctx, input.State)
	if err != nil {
		logger.From(ctx, s.logger).Error("bling: state de OAuth inválido ou já consumido",
			zap.String("state", input.State), zap.Error(err))
		return nil, httpx.ErrUnprocessable("Autorização expirada ou já utilizada. Tente conectar novamente.")
	}

	storeID := uuidToString(estado.StoreID)
	ctx = logger.WithStore(ctx, storeID, "")

	// SEM RETRY, deliberadamente. O code vale 1 minuto e uma segunda tentativa
	// com o mesmo code revoga o lojista — repetir não é uma chance a mais.
	creds, err := providererp.BlingExchangeCode(ctx, nil, clientID, clientSecret, input.Code)
	if err != nil {
		return nil, fmt.Errorf("trocando o code do Bling: %w", err)
	}

	// A identidade da conta é lida ANTES de gravar a integração: sem ela não há
	// chave de cota nem roteamento de webhook, e uma integração ativa sem conta
	// conhecida é uma loja que recebe evento de outra.
	prov, err := providererp.NewBling(providererp.BlingConfig{
		StoreID:      storeID,
		Credentials:  creds,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Logger:       s.logger,
	})
	if err != nil {
		return nil, err
	}
	empresa, err := prov.Empresa(ctx)
	if err != nil {
		return nil, fmt.Errorf("lendo a identidade da conta Bling: %w", err)
	}

	// Duas lojas LiveCart na MESMA empresa Bling dividiriam o teto de 3 req/s
	// sem saber uma da outra, e o webhook de URL única não teria como decidir
	// para qual entregar. O banco também recusa (uniq_integrations_erp_account),
	// mas recusar aqui dá uma mensagem que o lojista entende.
	if dono, err := s.repo.GetActiveERPByAccount(ctx, string(providers.ProviderBling), empresa.ID); err == nil &&
		dono != nil && dono.StoreID != storeID {
		return nil, httpx.ErrUnprocessable(
			"Esta conta Bling (" + empresa.Nome + ") já está conectada a outra loja do LiveCart.")
	}

	metadata := map[string]any{
		providers.MetadataBlingCompanyID: empresa.ID,
		"bling_company_name":             empresa.Nome,
		"bling_company_document":         empresa.CNPJ,
	}

	integracaoID, err := s.upsertBlingIntegration(ctx, storeID, creds, metadata)
	if err != nil {
		return nil, err
	}

	// A coluna é a fonte da verdade para o lookup indexado do webhook; o
	// metadata é espelho, para o factory não precisar de consulta a mais.
	if err := s.repo.SetERPAccountID(ctx, integracaoID, empresa.ID); err != nil {
		return nil, fmt.Errorf("gravando a identidade da conta Bling: %w", err)
	}

	logger.From(ctx, s.logger).Info("bling conectado",
		zap.String("integration_id", integracaoID),
		zap.String("bling_company_id", empresa.ID),
		zap.String("bling_company_name", empresa.Nome),
	)

	return &OAuthCallbackOutput{
		IntegrationID: integracaoID,
		StoreID:       storeID,
		Provider:      string(providers.ProviderBling),
		Status:        "active",
	}, nil
}

// upsertBlingIntegration cria ou atualiza a integração de ERP da loja.
//
// Reconecta a MESMA linha quando ela já existe (renovação de consentimento,
// troca de conta) em vez de criar outra: o índice uniq_integrations_store_one_erp
// recusaria a segunda, e o lojista veria um erro de banco em vez de reconectar.
func (s *Service) upsertBlingIntegration(
	ctx context.Context, storeID string, creds *providers.Credentials, metadata map[string]any,
) (string, error) {
	cifradas, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return "", fmt.Errorf("encrypting credentials: %w", err)
	}
	expira := creds.ExpiresAt

	existente, _ := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypeERP), string(providers.ProviderBling))
	if existente != nil {
		if err := s.repo.UpdateCredentials(ctx, existente.ID, cifradas, &expira); err != nil {
			return "", fmt.Errorf("updating credentials: %w", err)
		}
		if err := s.repo.UpdateMetadata(ctx, existente.ID, metadata); err != nil {
			return "", fmt.Errorf("updating metadata: %w", err)
		}
		if err := s.repo.UpdateStatus(ctx, existente.ID, "active"); err != nil {
			return "", fmt.Errorf("updating status: %w", err)
		}
		return existente.ID, nil
	}

	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:        storeID,
		Type:           string(providers.ProviderTypeERP),
		Provider:       string(providers.ProviderBling),
		Status:         "active",
		Credentials:    cifradas,
		TokenExpiresAt: &expira,
		Metadata:       metadata,
	})
	if err != nil {
		return "", fmt.Errorf("creating integration: %w", err)
	}
	return row.ID, nil
}
