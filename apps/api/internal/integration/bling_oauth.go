package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, "Aplicativo Bling não configurado no servidor")
	}

	ctx := logger.WithStore(context.Background(), storeID, "")

	// A regra de negócio antes de mandar o lojista para a tela do Bling: uma
	// loja integra UM ERP. Descobrir isso DEPOIS de autorizar seria fazer o
	// lojista percorrer a tela de consentimento para nada — e ainda gastaria uma
	// das vagas do teto de usuários do aplicativo não homologado.
	if err := s.checkBlingERPConnection(ctx, storeID); err != nil {
		return nil, err
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

func (s *Service) checkBlingERPConnection(ctx context.Context, storeID string) error {
	existing, err := s.repo.GetAnyByType(ctx, storeID, string(providers.ProviderTypeERP))
	if err != nil {
		return fmt.Errorf("verificando a integração de ERP da loja: %w", err)
	}
	if existing != nil && existing.Provider != string(providers.ProviderBling) {
		return httpx.DomainError(422, httpx.CodeValidationFailed,
			"Esta loja já usa o "+nomeAmigavelDoERP(existing.Provider)+
				". Só é possível manter um ERP conectado por vez — desconecte-o antes de conectar o Bling.")
	}
	return nil
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
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, "Aplicativo Bling não configurado no servidor")
	}

	// Consumo ATÔMICO: valida e apaga numa query só.
	//
	// Com o par GetOAuthState + defer Delete haveria uma janela em que dois
	// callbacks simultâneos (duplo clique, retry do navegador) passariam os dois
	// pela validação e tentariam trocar o MESMO code. A doc do Bling avisa que
	// reusar um code válido REVOGA o acesso do usuário — o custo de errar aqui
	// não é um erro na tela, é a loja desconectada.
	estado, err := s.repo.consumeBlingOAuthState(ctx, input.State)
	if err != nil {
		return nil, err
	}

	storeID := uuidToString(estado.StoreID)
	ctx = logger.WithStore(ctx, storeID, "")
	if err := s.checkBlingERPConnection(ctx, storeID); err != nil {
		return nil, err
	}

	// SEM RETRY, deliberadamente. O code vale 1 minuto e uma segunda tentativa
	// com o mesmo code revoga o lojista — repetir não é uma chance a mais.
	if err := s.factory.WaitBlingToken(ctx); err != nil {
		return nil, fmt.Errorf("aguardando cota de autenticação do Bling: %w", err)
	}
	creds, err := providererp.BlingExchangeCode(ctx, nil, clientID, clientSecret, input.Code)
	if err != nil {
		if observeErr := s.factory.ObserveBlingTokenError(ctx, err); observeErr != nil {
			return nil, errors.Join(err, observeErr)
		}
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
	if empresa == nil || strings.TrimSpace(empresa.ID) == "" {
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, "O Bling não retornou a identificação da empresa. Tente conectar novamente.")
	}
	empresa.ID = strings.TrimSpace(empresa.ID)

	// Duas lojas LiveCart na MESMA empresa Bling dividiriam o teto de 3 req/s
	// sem saber uma da outra, e o webhook de URL única não teria como decidir
	// para qual entregar. O banco também recusa (uniq_integrations_erp_account),
	// mas recusar aqui dá uma mensagem que o lojista entende.
	dono, err := s.repo.GetActiveERPByAccount(ctx, string(providers.ProviderBling), empresa.ID)
	if err != nil && !httpx.IsNotFound(err) {
		return nil, fmt.Errorf("verificando a loja da conta Bling: %w", err)
	}
	if dono != nil && dono.StoreID != storeID {
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed,
			"Esta conta Bling ("+empresa.Nome+") já está conectada a outra loja do LiveCart.")
	}

	// A regra de UM ERP tem de valer AQUI também, e não só no botão.
	//
	// getBlingOAuthURL já a checa, mas ele roda ANTES da tela de consentimento
	// do Bling — e entre uma coisa e outra cabe uma conexão de Tiny em outra
	// aba. Aplicá-la só lá deixa a janela aberta, e o resultado é uma loja com
	// dois ERPs ativos: um estado que nada no fluxo de pedido sabe resolver.
	if err := s.checkBlingERPConnection(ctx, storeID); err != nil {
		return nil, err
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
	accountID, _ := metadata[providers.MetadataBlingCompanyID].(string)
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", httpx.DomainError(422, httpx.CodeValidationFailed, "Não é possível conectar o Bling sem a identificação da empresa.")
	}
	if creds == nil || creds.AccessToken == "" {
		return "", httpx.DomainError(422, httpx.CodeValidationFailed, "Não é possível conectar o Bling sem credenciais válidas.")
	}
	cifradas, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return "", fmt.Errorf("encrypting credentials: %w", err)
	}
	// Only account identity comes from OAuth. Other settings are merged from
	// the current database row, never replaced with an earlier snapshot.
	identity := map[string]any{
		providers.MetadataBlingCompanyID: accountID,
		"bling_company_name":             metadata["bling_company_name"],
		"bling_company_document":         metadata["bling_company_document"],
	}
	return s.repo.saveBlingOAuthConnection(ctx, storeID, accountID, cifradas, creds.ExpiresAt, identity)
}
