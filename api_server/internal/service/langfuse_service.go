package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

type LangfuseConfig struct {
	DB *sql.DB
	// Optional Langfuse Postgres pool. Nil when unconfigured.
	LangfuseDB  *sql.DB
	Host        string
	AdminAPIKey string
}

type LangfuseService struct {
	repo             *repository.LangfuseRepo
	host             string
	adminAPIKey      string
	httpClient       *http.Client
	noRedirectClient *http.Client
}

// ErrLangfuseDBNotConfigured is re-exported from the repository so
// existing handler error checks (errors.Is(..., service.Err...)) keep
// working unchanged.
var ErrLangfuseDBNotConfigured = repository.ErrLangfuseDBNotConfigured

// LangfuseDBConfigured reports whether the api server was started with a
// Langfuse Postgres connection. Used by handlers to fail fast.
func (s *LangfuseService) LangfuseDBConfigured() bool {
	return s != nil && s.repo.LangfuseDBConfigured()
}

// orgMapping is an alias for the repo type so internal call sites
// (ensureOrgMapping etc.) keep their existing signatures.
type orgMapping = repository.LangfuseOrgMapping

func (s *LangfuseService) Host() string {
	if s == nil {
		return ""
	}
	return s.host
}

// ProjectKeys is an alias for the repo type so the LangfuseProvisioner
// interface and its mocks (provisioning_service*.go) keep working
// without churn.
type ProjectKeys = repository.LangfuseProjectKeys

// Conversation, ConversationDetail, ThreadMessage and TraceSpan are
// aliases so handlers/UI clients that already destructure on json
// field names keep working.
type (
	Conversation       = repository.Conversation
	ConversationDetail = repository.ConversationDetail
	ThreadMessage      = repository.ThreadMessage
	TraceSpan          = repository.TraceSpan
)

// ErrConversationNotFound is returned by GetConversation /
// GetMessageTrace when the (datapack, session_id) or
// (datapack, session_id, message_id) tuple has no rows in Langfuse —
// callers translate it into a 404.
var ErrConversationNotFound = errors.New("conversation not found")

var errObserveInvalidCredentials error = errors.New("observe credentials invalid")

func NewLangfuseService(cfg LangfuseConfig) *LangfuseService {
	return &LangfuseService{
		repo:        repository.NewLangfuseRepo(cfg.DB, cfg.LangfuseDB),
		host:        strings.TrimSpace(strings.TrimSuffix(cfg.Host, "/")),
		adminAPIKey: strings.TrimSpace(cfg.AdminAPIKey),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		noRedirectClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (s *LangfuseService) Enabled() bool {
	return s != nil &&
		s.repo != nil &&
		s.host != "" &&
		s.adminAPIKey != ""
}

func (s *LangfuseService) GenerateProjectKeysForDatapack(
	accountID string,
	accountEmail string,
	firstName string,
	lastName string,
	datapackID string,
	datapackName string,
) (*ProjectKeys, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("langfuse is not configured")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}

	existing, err := s.repo.GetProjectMapping(datapackID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	org, err := s.ensureOrgMapping(accountID, accountEmail)
	if err != nil {
		return nil, err
	}
	if err := s.ensureOrgUser(
		org,
		accountID,
		accountEmail,
		firstName,
		lastName,
	); err != nil {
		return nil, err
	}

	return s.ensureProjectMapping(accountID, datapackID, datapackName, org)
}

// ProvisionForDatapack ensures a langfuse_project_mapping row exists
// for (account, datapack). Best-effort: returns nil when Langfuse is
// disabled or upstream provisioning fails (errors are logged).
// Idempotent: a no-op when a mapping is already present.
func (s *LangfuseService) ProvisionForDatapack(
	account *models.Account,
	datapackID, datapackName string,
) *ProjectKeys {
	if !s.Enabled() || account == nil {
		return nil
	}
	keys, err := s.GenerateProjectKeysForDatapack(
		account.ID,
		account.Email,
		account.FirstName,
		account.LastName,
		datapackID,
		datapackName,
	)
	if err != nil {
		slog.Error("langfuse mapping provisioning failed",
			"user_id", account.ID,
			"datapack_id", datapackID,
			"datapack_name", datapackName,
			"error", err,
		)
		return nil
	}
	return keys
}

type ObserveBootstrapResult struct {
	SetCookies []string
	OrgID      string
}

func (s *LangfuseService) BootstrapObserveSession(
	accountID string,
	accountEmail string,
	firstName string,
	lastName string,
) (*ObserveBootstrapResult, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("langfuse is not configured")
	}
	accountID = strings.TrimSpace(accountID)
	accountEmail = strings.TrimSpace(accountEmail)
	if accountID == "" {
		return nil, fmt.Errorf("account_id is required")
	}
	if accountEmail == "" {
		return nil, fmt.Errorf("email is required")
	}

	org, err := s.ensureOrgMapping(accountID, accountEmail)
	if err != nil {
		return nil, err
	}
	if err := s.ensureOrgUser(
		org,
		accountID,
		accountEmail,
		firstName,
		lastName,
	); err != nil {
		return nil, err
	}
	loginEmail := langfuseLoginEmail(accountID, accountEmail)

	csrfToken, csrfSetCookies, cookieHeader, err := s.fetchObserveCSRF()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	loginSetCookies, err := s.observeCredentialsLogin(
		ctx,
		loginEmail,
		csrfToken,
		cookieHeader,
	)
	if err != nil {
		// Recover from stale SCIM credentials by re-creating org user once.
		if errors.Is(err, errObserveInvalidCredentials) {
			if resetErr := s.recreateOrgUser(
				org,
				accountID,
				accountEmail,
				firstName,
				lastName,
			); resetErr != nil {
				return nil, fmt.Errorf("%w; recreate scim user failed: %v", err, resetErr)
			}
			csrfToken, csrfSetCookies, cookieHeader, err = s.fetchObserveCSRF()
			if err != nil {
				return nil, err
			}
			loginSetCookies, err = s.observeCredentialsLogin(
				ctx,
				loginEmail,
				csrfToken,
				cookieHeader,
			)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	merged := append([]string{}, csrfSetCookies...)
	merged = append(merged, loginSetCookies...)
	return &ObserveBootstrapResult{
		SetCookies: merged,
		OrgID:      org.OrgID,
	}, nil
}

func (s *LangfuseService) ensureOrgMapping(accountID, accountEmail string) (*orgMapping, error) {
	existing, err := s.repo.GetOrgMapping(accountID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	orgName := fmt.Sprintf("meko-org-%s", langfuseShortID(accountID))
	orgResp, err := s.createOrg(orgName, accountID, accountEmail)
	if err != nil {
		if !strings.Contains(err.Error(), "status 409") {
			return nil, err
		}
		slog.Info("langfuse org already exists, looking up by name",
			"account_id", accountID, "org_name", orgName)
		orgResp, err = s.getOrgByName(orgName)
		if err != nil {
			return nil, fmt.Errorf("lookup existing langfuse org: %w", err)
		}
		if orgResp == nil {
			return nil, fmt.Errorf("langfuse org %q returned 409 but was not found by name", orgName)
		}
	}

	keyResp, err := s.createOrgAPIKey(orgResp.ID, accountID)
	if err != nil {
		return nil, err
	}
	slog.Info("langfuse org api keys created",
		"account_id", accountID,
		"org_id", orgResp.ID,
		"org_name", orgResp.Name,
		"org_public_key", keyResp.PublicKey,
		"org_secret_key", keyResp.SecretKey,
	)

	if err := s.repo.UpsertOrgMapping(accountID, orgResp.ID, orgResp.Name, keyResp.PublicKey, keyResp.SecretKey); err != nil {
		return nil, err
	}
	return s.repo.GetOrgMapping(accountID)
}

func (s *LangfuseService) ensureProjectMapping(
	accountID string,
	datapackID string,
	datapackName string,
	org *orgMapping,
) (*ProjectKeys, error) {
	projectName := strings.TrimSpace(datapackName)
	if projectName == "" {
		projectName = fmt.Sprintf("meko-%s", langfuseShortID(accountID))
	}

	projectResp, err := s.createProject(org.PublicKey, org.SecretKey, projectName, accountID, datapackID, datapackName)
	if err != nil {
		if strings.Contains(err.Error(), "status 409") {
			sfx, sfxErr := randomHexSuffix(4)
			if sfxErr != nil {
				return nil, fmt.Errorf("generate project suffix: %w", sfxErr)
			}
			uniqueProjectName := fmt.Sprintf("%s-%s", projectName, sfx)
			projectResp, err = s.createProject(
				org.PublicKey,
				org.SecretKey,
				uniqueProjectName,
				accountID,
				datapackID,
				datapackName,
			)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	keyResp, err := s.createProjectAPIKey(org.PublicKey, org.SecretKey, projectResp.ID, accountID)
	if err != nil {
		return nil, err
	}
	slog.Info("langfuse project api keys created",
		"account_id", accountID,
		"datapack_id", datapackID,
		"project_id", projectResp.ID,
		"project_name", projectResp.Name,
		"project_public_key", keyResp.PublicKey,
		"project_secret_key", keyResp.SecretKey,
	)

	if err := s.repo.UpsertProjectMapping(datapackID, projectResp.ID, projectResp.Name, keyResp.PublicKey, keyResp.SecretKey); err != nil {
		return nil, err
	}
	return s.repo.GetProjectMapping(datapackID)
}

func (s *LangfuseService) GetOrgKeysForAccount(accountID string) (string, string, error) {
	if !s.Enabled() {
		return "", "", fmt.Errorf("langfuse is not configured")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", "", fmt.Errorf("account_id is required")
	}
	org, err := s.repo.GetOrgMapping(accountID)
	if err != nil {
		return "", "", err
	}
	if org == nil {
		return "", "", fmt.Errorf("langfuse org mapping not found")
	}
	return org.PublicKey, org.SecretKey, nil
}

// ListConversations returns one page of Langfuse sessions for the
// datapack's project (newest first), plus the resolved langfuse
// project_id so the UI can build per-conversation deep links without
// a second roundtrip. Empty projectID means no mapping exists.
func (s *LangfuseService) ListConversations(
	datapackID string,
	limit, offset int,
) (string, []Conversation, error) {
	if s == nil || s.repo == nil {
		return "", nil, fmt.Errorf("langfuse service is not configured")
	}
	if !s.LangfuseDBConfigured() {
		return "", nil, ErrLangfuseDBNotConfigured
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return "", nil, fmt.Errorf("datapack_id is required")
	}

	mapping, err := s.repo.GetProjectMapping(datapackID)
	if err != nil {
		return "", nil, err
	}
	if mapping == nil {
		return "", []Conversation{}, nil
	}
	projectID := strings.TrimSpace(mapping.ProjectID)
	if projectID == "" {
		slog.Error("langfuse project mapping is incomplete",
			"datapack_id", datapackID)
		return "", nil, fmt.Errorf("langfuse project mapping is incomplete")
	}

	conversations, err := s.repo.ListConversationsForProject(projectID, limit, offset)
	if err != nil {
		return "", nil, err
	}

	// Overlay authoritative persisted-memory counts from
	// datapacks.meko_mem0_collection. The list query above sets
	// MemoriesAdded to 0; we replace it with the mem0 row count so the
	// number matches the Memories tab. Failure here is logged but
	// non-fatal — the rest of the listing is still useful.
	if len(conversations) > 0 {
		ids := make([]string, len(conversations))
		for i, c := range conversations {
			ids[i] = c.ID
		}
		counts, mErr := s.repo.MemoriesPerConversation(datapackID, ids)
		if mErr != nil {
			slog.Warn("conversations: failed to fetch mem0 counts; defaulting to 0",
				"datapack_id", datapackID, "err", mErr)
		} else {
			for i := range conversations {
				conversations[i].MemoriesAdded = counts[conversations[i].ID]
			}
		}
	}

	return projectID, conversations, nil
}

// CountConversations returns the total number of distinct Langfuse
// sessions (= chat conversations) for the (user, datapack)'s Langfuse
// project. Uses the same filters as ListConversations so the count
// agrees with whatever the listing endpoint can paginate through.
//
// Returns (0, nil) when no Langfuse mapping exists for the datapack;
// ErrLangfuseDBNotConfigured when the api server was started without
// --langfuse-db-host. Datapack ownership is enforced upstream.
func (s *LangfuseService) CountConversations(
	datapackID string,
) (int, error) {
	if s == nil || s.repo == nil {
		return 0, fmt.Errorf("langfuse service is not configured")
	}
	if !s.LangfuseDBConfigured() {
		return 0, ErrLangfuseDBNotConfigured
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return 0, fmt.Errorf("datapack_id is required")
	}

	mapping, err := s.repo.GetProjectMapping(datapackID)
	if err != nil {
		return 0, err
	}
	if mapping == nil {
		return 0, nil
	}
	projectID := strings.TrimSpace(mapping.ProjectID)
	if projectID == "" {
		return 0, fmt.Errorf("langfuse project mapping is incomplete")
	}

	n, err := s.repo.CountDistinctSessionsForProject(projectID)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// GetConversation returns the header + a paginated thread for a
// single conversation under the datapack's Langfuse project.
//
// Returns ErrConversationNotFound when no Langfuse mapping exists for
// the datapack or no rows match the session_id; callers map that to
// HTTP 404. Datapack access is enforced upstream by
// RequireDatapackAccess.
func (s *LangfuseService) GetConversation(
	datapackID, conversationID string,
	messagesLimit, messagesOffset int,
) (string, *ConversationDetail, error) {
	if s == nil || s.repo == nil {
		return "", nil, fmt.Errorf("langfuse service is not configured")
	}
	if !s.LangfuseDBConfigured() {
		return "", nil, ErrLangfuseDBNotConfigured
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return "", nil, fmt.Errorf("datapack_id is required")
	}
	conversationID = normalizeSessionID(conversationID)
	if conversationID == "" {
		return "", nil, fmt.Errorf("conversation_id is required")
	}

	mapping, err := s.repo.GetProjectMapping(datapackID)
	if err != nil {
		return "", nil, err
	}
	if mapping == nil {
		return "", nil, ErrConversationNotFound
	}
	projectID := strings.TrimSpace(mapping.ProjectID)
	if projectID == "" {
		return "", nil, fmt.Errorf("langfuse project mapping is incomplete")
	}

	detail, err := s.repo.GetConversationThread(
		projectID, conversationID, datapackID, messagesLimit, messagesOffset,
	)
	if err != nil {
		return "", nil, err
	}
	if detail == nil {
		return projectID, nil, ErrConversationNotFound
	}
	
	counts, mErr := s.repo.MemoriesPerConversation(datapackID, []string{conversationID})
	if mErr != nil {
		slog.Warn("conversation detail: failed to fetch mem0 count; defaulting to 0",
			"datapack_id", datapackID, "conversation_id", conversationID, "err", mErr)
	} else {
		detail.MemoriesAdded = counts[conversationID]
	}

	return projectID, detail, nil
}

// GetMessageTrace returns the windowed observation tree for one
// message turn — used by the conversation viewer's trace drawer.
//
// Returns ErrConversationNotFound when there's no Langfuse mapping or
// no message with the given id under the conversation. Datapack access
// is enforced upstream by RequireDatapackAccess.
func (s *LangfuseService) GetMessageTrace(
	datapackID, conversationID, messageID string,
) (string, []TraceSpan, error) {
	if s == nil || s.repo == nil {
		return "", nil, fmt.Errorf("langfuse service is not configured")
	}
	if !s.LangfuseDBConfigured() {
		return "", nil, ErrLangfuseDBNotConfigured
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return "", nil, fmt.Errorf("datapack_id is required")
	}

	conversationID = normalizeSessionID(conversationID)
	if conversationID == "" {
		return "", nil, fmt.Errorf("conversation_id is required")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return "", nil, fmt.Errorf("message_id is required")
	}

	mapping, err := s.repo.GetProjectMapping(datapackID)
	if err != nil {
		return "", nil, err
	}
	if mapping == nil {
		return "", nil, ErrConversationNotFound
	}
	projectID := strings.TrimSpace(mapping.ProjectID)
	if projectID == "" {
		return "", nil, fmt.Errorf("langfuse project mapping is incomplete")
	}

	spans, err := s.repo.GetMessageTrace(projectID, conversationID, messageID)
	if err != nil {
		return "", nil, err
	}
	if spans == nil {
		return projectID, nil, ErrConversationNotFound
	}
	return projectID, spans, nil
}

// normalizeSessionID strips hyphens from the input; MCP's
// langfuse_store derives Langfuse trace_ids from session_ids by
// removing hyphens, so the on-disk session_id is always the unhyphenated
// form. Callers may receive either form from the URL — normalize here
// so handlers don't have to.
func normalizeSessionID(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "-", "")
}

type createOrgReq struct {
	Name     string                 `json:"name"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type createOrgResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *LangfuseService) createOrg(name, accountID, accountEmail string) (*createOrgResp, error) {
	body := createOrgReq{
		Name: name,
		Metadata: map[string]interface{}{
			"source":     "meko",
			"account_id": accountID,
			"email":      accountEmail,
		},
	}
	var out createOrgResp
	if err := s.doJSON(http.MethodPost, "/api/admin/organizations", bearerAuth(s.adminAPIKey), body, &out); err != nil {
		return nil, fmt.Errorf("create langfuse org: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("create langfuse org: missing id in response")
	}
	return &out, nil
}

type listOrgsResp struct {
	Data []createOrgResp `json:"data"`
}

// getOrgByName lists all Langfuse orgs and returns the one matching the given name.
// Returns (nil, nil) if no org with that name exists.
func (s *LangfuseService) getOrgByName(name string) (*createOrgResp, error) {
	var out listOrgsResp
	if err := s.doJSON(http.MethodGet, "/api/admin/organizations", bearerAuth(s.adminAPIKey), nil, &out); err != nil {
		return nil, fmt.Errorf("list langfuse orgs: %w", err)
	}
	for i := range out.Data {
		if out.Data[i].Name == name {
			return &out.Data[i], nil
		}
	}
	return nil, nil
}

type createAPIKeyReq struct {
	Note string `json:"note"`
}

type createAPIKeyResp struct {
	PublicKey string `json:"publicKey"`
	SecretKey string `json:"secretKey"`
}

func (s *LangfuseService) createOrgAPIKey(orgID, accountID string) (*createAPIKeyResp, error) {
	body := createAPIKeyReq{Note: fmt.Sprintf("meko-org-%s", langfuseShortID(accountID))}
	var out createAPIKeyResp
	path := fmt.Sprintf("/api/admin/organizations/%s/apiKeys", orgID)
	if err := s.doJSON(http.MethodPost, path, bearerAuth(s.adminAPIKey), body, &out); err != nil {
		return nil, fmt.Errorf("create langfuse org api key: %w", err)
	}
	if out.PublicKey == "" || out.SecretKey == "" {
		return nil, fmt.Errorf("create langfuse org api key: missing keys in response")
	}
	return &out, nil
}

type createProjectReq struct {
	Name      string                 `json:"name"`
	Retention int                    `json:"retention"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

type createProjectResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *LangfuseService) createProject(orgPublicKey, orgSecretKey, projectName, accountID, datapackID, datapackName string) (*createProjectResp, error) {
	body := createProjectReq{
		Name:      projectName,
		Retention: 0,
		Metadata: map[string]interface{}{
			"source":        "meko",
			"account_id":    accountID,
			"datapack_id":   datapackID,
			"datapack_name": datapackName,
		},
	}
	var out createProjectResp
	if err := s.doJSON(http.MethodPost, "/api/public/projects", basicAuth(orgPublicKey, orgSecretKey), body, &out); err != nil {
		return nil, fmt.Errorf("create langfuse project: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("create langfuse project: missing id in response")
	}
	return &out, nil
}

func (s *LangfuseService) createProjectAPIKey(orgPublicKey, orgSecretKey, projectID, accountID string) (*createAPIKeyResp, error) {
	body := createAPIKeyReq{Note: fmt.Sprintf("meko-project-%s", langfuseShortID(accountID))}
	var out createAPIKeyResp
	path := fmt.Sprintf("/api/public/projects/%s/apiKeys", projectID)
	if err := s.doJSON(http.MethodPost, path, basicAuth(orgPublicKey, orgSecretKey), body, &out); err != nil {
		return nil, fmt.Errorf("create langfuse project api key: %w", err)
	}
	if out.PublicKey == "" || out.SecretKey == "" {
		return nil, fmt.Errorf("create langfuse project api key: missing keys in response")
	}
	return &out, nil
}

type scimUserName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimUserEmail struct {
	Primary bool   `json:"primary"`
	Value   string `json:"value"`
	Type    string `json:"type"`
}

type scimUserReq struct {
	UserName string          `json:"userName"`
	Name     scimUserName    `json:"name"`
	Emails   []scimUserEmail `json:"emails"`
	Active   bool            `json:"active"`
	Password string          `json:"password"`
	Roles    []string        `json:"roles"`
}

type scimUserPutReq struct {
	Schemas []string `json:"schemas"`
	Active  bool     `json:"active"`
	Roles   []string `json:"roles"`
}

func buildScimUserReq(loginEmail, firstName, lastName, fallbackEmail, adminAPIKey string) scimUserReq {
	first := strings.TrimSpace(firstName)
	last := strings.TrimSpace(lastName)
	fullName := strings.TrimSpace(first + " " + last)
	if fullName == "" {
		fullName = fallbackEmail
	}
	return scimUserReq{
		UserName: loginEmail,
		Name:     scimUserName{Formatted: fullName, GivenName: first, FamilyName: last},
		Emails:   []scimUserEmail{{Primary: true, Value: loginEmail, Type: "work"}},
		Active:   true,
		Password: deriveLangfusePassword(loginEmail, adminAPIKey),
		Roles:    []string{"OWNER"},
	}
}

func (s *LangfuseService) ensureOrgUser(
	org *orgMapping,
	accountID string,
	accountEmail string,
	firstName string,
	lastName string,
) error {
	if org == nil {
		return fmt.Errorf("langfuse org mapping is required")
	}
	email := strings.TrimSpace(accountEmail)
	if email == "" {
		return fmt.Errorf("email is required for langfuse user provisioning")
	}
	loginEmail := langfuseLoginEmail(accountID, email)
	body := buildScimUserReq(loginEmail, firstName, lastName, email, s.adminAPIKey)
	if err := s.createOrgUser(org, body, true); err != nil {
		return err
	}
	return s.ensureOrgUserRole(org, loginEmail, "OWNER")
}

func (s *LangfuseService) createOrgUser(
	org *orgMapping,
	requestBody scimUserReq,
	allowConflict bool,
) error {
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal scim user request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		s.host+"/api/public/scim/Users",
		bytes.NewReader(raw),
	)
	if err != nil {
		return fmt.Errorf("build scim user request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+basicAuth(org.PublicKey, org.SecretKey).token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create langfuse org user: send request: %w", err)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	trimmed := strings.TrimSpace(string(respRaw))
	if allowConflict &&
		(resp.StatusCode == http.StatusConflict ||
			strings.Contains(strings.ToLower(trimmed), "already exists")) {
		return nil
	}
	return fmt.Errorf("create langfuse org user: status %d: %s", resp.StatusCode, trimmed)
}

type scimUsersListResponse struct {
	Resources []struct {
		ID       string `json:"id"`
		UserName string `json:"userName"`
	} `json:"Resources"`
}

func (s *LangfuseService) listOrgUserIDsByEmail(org *orgMapping, email string) ([]string, error) {
	filter := url.QueryEscape(fmt.Sprintf("userName eq \"%s\"", email))
	path := "/api/public/scim/Users?filter=" + filter
	var out scimUsersListResponse
	if err := s.doJSON(http.MethodGet, path, basicAuth(org.PublicKey, org.SecretKey), nil, &out); err != nil {
		return nil, fmt.Errorf("list langfuse scim users: %w", err)
	}
	ids := make([]string, 0, len(out.Resources))
	for _, r := range out.Resources {
		if strings.EqualFold(strings.TrimSpace(r.UserName), strings.TrimSpace(email)) &&
			strings.TrimSpace(r.ID) != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func (s *LangfuseService) deleteOrgUserByID(org *orgMapping, userID string) error {
	path := "/api/public/scim/Users/" + userID
	if err := s.doJSON(http.MethodDelete, path, basicAuth(org.PublicKey, org.SecretKey), nil, nil); err != nil {
		return fmt.Errorf("delete langfuse scim user %s: %w", userID, err)
	}
	return nil
}

func (s *LangfuseService) recreateOrgUser(
	org *orgMapping,
	accountID string,
	accountEmail string,
	firstName string,
	lastName string,
) error {
	email := strings.TrimSpace(accountEmail)
	if org == nil || email == "" {
		return fmt.Errorf("org mapping and email are required")
	}
	loginEmail := langfuseLoginEmail(accountID, email)
	userIDs, err := s.listOrgUserIDsByEmail(org, loginEmail)
	if err != nil {
		return err
	}
	for _, id := range userIDs {
		if err := s.deleteOrgUserByID(org, id); err != nil {
			return err
		}
	}

	body := buildScimUserReq(loginEmail, firstName, lastName, email, s.adminAPIKey)
	if err := s.createOrgUser(org, body, false); err != nil {
		return err
	}
	return s.ensureOrgUserRole(org, loginEmail, "OWNER")
}

func (s *LangfuseService) ensureOrgUserRole(
	org *orgMapping,
	email string,
	role string,
) error {
	if org == nil {
		return fmt.Errorf("org mapping is required")
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return fmt.Errorf("role is required")
	}
	userIDs, err := s.listOrgUserIDsByEmail(org, email)
	if err != nil {
		return err
	}
	if len(userIDs) == 0 {
		return fmt.Errorf("scim user not found for email %s", email)
	}
	body := scimUserPutReq{
		Schemas: []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		Active:  true,
		Roles:   []string{role},
	}
	for _, id := range userIDs {
		path := "/api/public/scim/Users/" + id
		if err := s.doJSON(
			http.MethodPut,
			path,
			basicAuth(org.PublicKey, org.SecretKey),
			body,
			nil,
		); err != nil {
			return fmt.Errorf("set scim user role for %s: %w", id, err)
		}
	}
	return nil
}

func (s *LangfuseService) fetchObserveCSRF() (string, []string, string, error) {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		s.host+"/api/auth/csrf",
		nil,
	)
	if err != nil {
		return "", nil, "", fmt.Errorf("build csrf request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", nil, "", fmt.Errorf("fetch csrf token: send request: %w", err)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, "", fmt.Errorf(
			"fetch csrf token: status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(respRaw)),
		)
	}
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(respRaw, &payload); err != nil {
		return "", nil, "", fmt.Errorf("fetch csrf token: decode response: %w", err)
	}
	if strings.TrimSpace(payload.CSRFToken) == "" {
		return "", nil, "", fmt.Errorf("fetch csrf token: csrfToken is missing")
	}
	cookiePairs := make([]string, 0, len(resp.Cookies()))
	for _, c := range resp.Cookies() {
		cookiePairs = append(cookiePairs, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	return payload.CSRFToken, resp.Header.Values("Set-Cookie"), strings.Join(cookiePairs, "; "), nil
}

func (s *LangfuseService) observeCredentialsLogin(
	ctx context.Context,
	loginEmail string,
	csrfToken string,
	cookieHeader string,
) ([]string, error) {
	form := url.Values{
		"email":       {strings.TrimSpace(loginEmail)},
		"password":    {deriveLangfusePassword(loginEmail, s.adminAPIKey)},
		"csrfToken":   {csrfToken},
		"json":        {"true"},
		"callbackUrl": {"/"},
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.host+"/api/auth/callback/credentials",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("build observe credentials login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}

	// NextAuth credentials callback usually responds with redirect status;
	// noRedirectClient preserves that response so we can forward cookies.
	resp, err := s.noRedirectClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("observe credentials login: send request: %w", err)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		body := strings.TrimSpace(string(respRaw))
		lowerBody := strings.ToLower(body)
		if resp.StatusCode == http.StatusUnauthorized &&
			(strings.Contains(lowerBody, "invalid%20credentials") ||
				strings.Contains(lowerBody, "invalid credentials") ||
				strings.Contains(lowerBody, "credentialssignin")) {
			return nil, fmt.Errorf("%w: status %d: %s", errObserveInvalidCredentials, resp.StatusCode, body)
		}
		return nil, fmt.Errorf(
			"observe credentials login: status %d: %s",
			resp.StatusCode,
			body,
		)
	}
	if strings.Contains(string(respRaw), "CredentialsSignin") {
		return nil, fmt.Errorf("%w: credentials callback rejected login", errObserveInvalidCredentials)
	}
	return resp.Header.Values("Set-Cookie"), nil
}

type requestAuth struct {
	kind  string
	token string
}

func bearerAuth(token string) requestAuth {
	return requestAuth{kind: "bearer", token: token}
}

func basicAuth(user, pass string) requestAuth {
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return requestAuth{kind: "basic", token: cred}
}

func (s *LangfuseService) doJSON(method, path string, auth requestAuth, reqBody interface{}, out interface{}) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	url := s.host + path
	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch auth.kind {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+auth.token)
	case "basic":
		req.Header.Set("Authorization", "Basic "+auth.token)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respRaw)))
	}
	if out == nil || len(respRaw) == 0 {
		return nil
	}
	if err := json.Unmarshal(respRaw, out); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}

func langfuseShortID(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}

func randomHexSuffix(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func deriveLangfusePassword(email, adminAPIKey string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(
		fmt.Sprintf("meko:%s:%s", email, adminAPIKey),
	))))
	return "Meko#" + hex.EncodeToString(sum[:])[:24] + "aA1!"
}

func langfuseLoginEmail(accountID, accountEmail string) string {
	email := strings.TrimSpace(accountEmail)
	if email != "" {
		return email
	}
	return fmt.Sprintf("meko-%s@example.local", langfuseShortID(accountID))
}
