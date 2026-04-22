package console

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// AuthStore persists the email → (bcrypt hash, tenant ID) mapping the
// B2C self-service signup / login flow needs. It is deliberately
// separate from TenantStore: production swaps this out for a Postgres
// table with an email uniqueness constraint and an audit-logged
// password column, while TenantStore carries the canonical tenant
// record the authenticator already consults.
type AuthStore interface {
	// CreateUser stores a new (email, bcrypt hash, tenant ID)
	// mapping. Implementations must reject duplicate emails.
	CreateUser(email, passwordHash, tenantID string) error

	// LookupUser returns the bcrypt hash and tenant ID bound to
	// email. Callers compare the hash against the user-supplied
	// password via bcrypt.CompareHashAndPassword.
	LookupUser(email string) (passwordHash, tenantID string, ok bool)
}

// TokenStore maps opaque bearer tokens to tenant IDs. It is used by
// the SPA to authenticate subsequent requests without resending
// email / password. Production replaces this with a JWT issuer or a
// signed session cookie; the Phase 3 scaffold keeps it in memory so
// the frontend can round-trip without a database dependency.
type TokenStore interface {
	// IssueToken mints a new token for tenantID and returns it.
	IssueToken(tenantID string) (string, error)

	// ResolveToken returns the tenant ID bound to token, or
	// (_, false) if no such token exists.
	ResolveToken(token string) (tenantID string, ok bool)
}

// MemoryAuthStore is a process-local AuthStore suitable for the
// Phase 3 console scaffold and tests.
type MemoryAuthStore struct {
	mu    sync.RWMutex
	users map[string]memoryAuthRow
}

type memoryAuthRow struct {
	PasswordHash string
	TenantID     string
}

// NewMemoryAuthStore returns an empty store.
func NewMemoryAuthStore() *MemoryAuthStore {
	return &MemoryAuthStore{users: map[string]memoryAuthRow{}}
}

// CreateUser implements AuthStore.
func (s *MemoryAuthStore) CreateUser(email, passwordHash, tenantID string) error {
	if email == "" {
		return errors.New("console: email is required")
	}
	key := strings.ToLower(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[key]; ok {
		return fmt.Errorf("console: email %q is already registered", email)
	}
	s.users[key] = memoryAuthRow{PasswordHash: passwordHash, TenantID: tenantID}
	return nil
}

// LookupUser implements AuthStore.
func (s *MemoryAuthStore) LookupUser(email string) (string, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.users[strings.ToLower(email)]
	if !ok {
		return "", "", false
	}
	return row.PasswordHash, row.TenantID, true
}

// MemoryTokenStore is a process-local TokenStore.
type MemoryTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]string
}

// NewMemoryTokenStore returns an empty store.
func NewMemoryTokenStore() *MemoryTokenStore {
	return &MemoryTokenStore{tokens: map[string]string{}}
}

// IssueToken mints a 32-byte random hex token and binds it to
// tenantID.
func (s *MemoryTokenStore) IssueToken(tenantID string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand token: %w", err)
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = tenantID
	return token, nil
}

// ResolveToken implements TokenStore.
func (s *MemoryTokenStore) ResolveToken(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokens[token]
	return id, ok
}

// AuthHooks collects the optional production integrations the signup
// flow needs to wire up before going live. All hooks are no-ops in
// the Phase 3 scaffold; the TODO comments below capture what each
// integration must eventually do.
type AuthHooks struct {
	// VerifyCAPTCHA validates the CAPTCHA token submitted with the
	// signup payload. A nil hook skips CAPTCHA verification — the
	// Phase 3 scaffold default.
	//
	// TODO(production): wire this to a real CAPTCHA provider
	// (hCaptcha / reCAPTCHA) and reject signups whose token fails
	// verification.
	VerifyCAPTCHA func(token string) error

	// SendVerificationEmail is called after a successful signup so
	// the user can verify their email. A nil hook skips the email
	// — the Phase 3 scaffold default.
	//
	// TODO(production): integrate with the transactional email
	// provider, gate the tenant's first S3 PUT on verification, and
	// expire unverified tenants after 7 days.
	SendVerificationEmail func(email, tenantID string) error
}

// AuthConfig collects the dependencies the auth handler needs. It
// layers on top of Config so the tenant-console handler can mount
// the signup / login endpoints on the same mux.
type AuthConfig struct {
	Tenants TenantStore
	Auth    AuthStore
	Tokens  TokenStore

	// NewTenantID returns a fresh tenant ID. Defaults to a 16-byte
	// hex-encoded identifier prefixed with "t-".
	NewTenantID func() (string, error)

	// GenerateKey mints an access/secret pair for the initial API
	// key. Defaults to the same 20/40-hex generator the console
	// handler uses.
	GenerateKey KeyGenerator

	// Hooks are optional production integrations (CAPTCHA, email).
	Hooks AuthHooks

	// Now returns the current time. Defaults to time.Now.
	Now Clock
}

// signupRequest is the payload accepted by POST /api/v1/auth/signup.
type signupRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	TenantName   string `json:"tenantName"`
	CAPTCHAToken string `json:"captchaToken,omitempty"`
	// OAuthToken is reserved for the OAuth signup variant. When
	// set, Password may be empty; the handler resolves the token
	// against the configured OAuth provider.
	//
	// TODO(production): accept a provider identifier
	// (google / microsoft) alongside the token and verify it.
	OAuthToken string `json:"oauthToken,omitempty"`
}

// TenantSummary is the tenant subset returned to the SPA on signup /
// login. It matches frontend/src/api/types.ts `Tenant`.
type TenantSummary struct {
	ID                         string                 `json:"id"`
	Name                       string                 `json:"name"`
	ContractType               tenant.ContractType    `json:"contractType"`
	LicenseTier                tenant.LicenseTier     `json:"licenseTier"`
	PlacementDefaultPolicyRef  string                 `json:"placementDefaultPolicyRef"`
	Budgets                    TenantBudgetsSummary   `json:"budgets"`
}

// TenantBudgetsSummary is the budgets slice the frontend dashboard
// renders. The full tenant.Budgets structure has additional operator
// knobs that should not leak to the SPA.
type TenantBudgetsSummary struct {
	EgressTBMonth float64 `json:"egressTbMonth"`
}

// AuthResponse is returned from POST /auth/signup and POST
// /auth/login. SecretKey is only populated on signup — the login
// endpoint intentionally omits it so a replayed login cannot be used
// to exfiltrate the tenant's S3 secret.
type AuthResponse struct {
	Tenant    TenantSummary `json:"tenant"`
	Token     string        `json:"token"`
	AccessKey string        `json:"accessKey,omitempty"`
	SecretKey string        `json:"secretKey,omitempty"`
	CreatedAt time.Time     `json:"createdAt,omitempty"`
}

// AuthHandler routes POST /api/v1/auth/signup and POST
// /api/v1/auth/login. It is mounted by Handler.Register alongside the
// tenant / usage / placement routes.
type AuthHandler struct {
	cfg AuthConfig
}

// NewAuthHandler returns an AuthHandler with defaults filled in.
func NewAuthHandler(cfg AuthConfig) *AuthHandler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GenerateKey == nil {
		cfg.GenerateKey = defaultKeyGenerator
	}
	if cfg.NewTenantID == nil {
		cfg.NewTenantID = defaultTenantIDGenerator
	}
	if cfg.Tokens == nil {
		cfg.Tokens = NewMemoryTokenStore()
	}
	return &AuthHandler{cfg: cfg}
}

// Register mounts the auth routes on mux under /api/v1/auth/.
func (h *AuthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/auth/", h.dispatch)
}

// ServeHTTP lets callers attach the handler directly.
func (h *AuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.dispatch(w, r)
}

const (
	authPathSignup = "/api/v1/auth/signup"
	authPathLogin  = "/api/v1/auth/login"
)

// maxAuthBodyBytes caps the request body the auth endpoints decode.
// Signup / login payloads are small JSON documents (email, password,
// tenant name, optional CAPTCHA / OAuth token); 16 KiB is three
// orders of magnitude above a realistic payload and keeps a
// pathological or hostile client from exhausting gateway memory by
// streaming a large JSON body at the public auth surface.
const maxAuthBodyBytes int64 = 16 * 1024

func (h *AuthHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case authPathSignup:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.signup(w, r)
	case authPathLogin:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.login(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown auth path "+r.URL.Path)
	}
}

func (h *AuthHandler) signup(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tenants == nil || h.cfg.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("signup payload exceeds %d bytes", maxAuthBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "decode signup: "+err.Error())
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.TenantName = strings.TrimSpace(req.TenantName)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "email is required and must contain '@'")
		return
	}
	if req.OAuthToken == "" && len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if req.TenantName == "" {
		req.TenantName = req.Email
	}

	// CAPTCHA gate (no-op by default; see AuthHooks.VerifyCAPTCHA).
	if h.cfg.Hooks.VerifyCAPTCHA != nil {
		if err := h.cfg.Hooks.VerifyCAPTCHA(req.CAPTCHAToken); err != nil {
			writeError(w, http.StatusForbidden, "captcha verification failed: "+err.Error())
			return
		}
	}

	// Short-circuit duplicate emails before we mint a tenant ID so
	// a retried signup does not litter the tenant store with
	// orphan records.
	if _, _, exists := h.cfg.Auth.LookupUser(req.Email); exists {
		writeError(w, http.StatusConflict, "email is already registered")
		return
	}

	tenantID, err := h.cfg.NewTenantID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint tenant id: "+err.Error())
		return
	}
	newTenant := defaultB2CTenant(tenantID, req.TenantName)
	if err := h.cfg.Tenants.CreateTenant(newTenant); err != nil {
		writeError(w, http.StatusInternalServerError, "create tenant: "+err.Error())
		return
	}

	var passwordHash string
	if req.OAuthToken == "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "hash password: "+err.Error())
			return
		}
		passwordHash = string(hash)
	}
	// OAuth branch: passwordHash stays empty and the login flow
	// refuses password logins for this user until the OAuth
	// provider is wired in.
	//
	// TODO(production): resolve req.OAuthToken against the
	// configured OAuth provider (Google / Microsoft) and store the
	// provider-issued subject identifier in AuthStore so subsequent
	// logins can be re-authenticated without a password.
	if err := h.cfg.Auth.CreateUser(req.Email, passwordHash, tenantID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	accessKey, secretKey, err := h.cfg.GenerateKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	if err := h.cfg.Tenants.AddAPIKey(tenantID, accessKey, secretKey); err != nil {
		writeError(w, http.StatusInternalServerError, "register key: "+err.Error())
		return
	}

	// Kick off the verification email (no-op by default; see
	// AuthHooks.SendVerificationEmail).
	if h.cfg.Hooks.SendVerificationEmail != nil {
		if err := h.cfg.Hooks.SendVerificationEmail(req.Email, tenantID); err != nil {
			// Signup still succeeds: the user can re-request
			// verification from the dashboard. Callers that
			// want hard-fail semantics can swap the hook for
			// one that panics or returns a different error.
			//
			// TODO(production): surface email-send failures
			// through a background retry queue with an alert
			// when the dead-letter rate exceeds 1 %.
			_ = err
		}
	}

	token, err := h.cfg.Tokens.IssueToken(tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "issue token: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, AuthResponse{
		Tenant:    summarizeTenant(newTenant),
		Token:     token,
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: h.cfg.Now(),
	})
}

// loginRequest is the payload accepted by POST /api/v1/auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) login(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tenants == nil || h.cfg.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("login payload exceeds %d bytes", maxAuthBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "decode login: "+err.Error())
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}
	hash, tenantID, ok := h.cfg.Auth.LookupUser(req.Email)
	if !ok || hash == "" {
		// Return a uniform 401 for missing user, OAuth-only
		// accounts, and wrong-password cases so a probing caller
		// can't distinguish them.
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	t, ok := h.cfg.Tenants.LookupTenant(tenantID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "tenant record missing for user")
		return
	}
	token, err := h.cfg.Tokens.IssueToken(tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "issue token: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AuthResponse{
		Tenant: summarizeTenant(t),
		Token:  token,
	})
}

// defaultB2CTenant builds the Tenant record written for a new
// self-service signup. The budgets mirror the pooled B2C tier
// described in docs/PROPOSAL.md §5.5 and serve as the default floor
// until the operator customises them.
func defaultB2CTenant(id, name string) tenant.Tenant {
	return tenant.Tenant{
		ID:           id,
		Name:         name,
		ContractType: tenant.ContractB2CPooled,
		LicenseTier:  tenant.LicenseStandard,
		Keys: tenant.Keys{
			RootKeyRef: "cmk://shared/b2c",
			DEKPolicy:  tenant.DEKPerObject,
		},
		PlacementDefault: tenant.PlacementDefault{PolicyRef: "b2c_pooled_default"},
		Budgets: tenant.Budgets{
			EgressTBMonth:  1.0,
			RequestsPerSec: 50,
		},
		Billing: tenant.Billing{Currency: "USD"},
	}
}

func summarizeTenant(t tenant.Tenant) TenantSummary {
	return TenantSummary{
		ID:                        t.ID,
		Name:                      t.Name,
		ContractType:              t.ContractType,
		LicenseTier:               t.LicenseTier,
		PlacementDefaultPolicyRef: t.PlacementDefault.PolicyRef,
		Budgets: TenantBudgetsSummary{
			EgressTBMonth: t.Budgets.EgressTBMonth,
		},
	}
}

// defaultTenantIDGenerator mints a 16-byte hex-encoded identifier
// prefixed with "t-". The prefix survives copy-paste into operator
// tooling and makes tenant IDs easy to grep in logs.
func defaultTenantIDGenerator() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand tenant id: %w", err)
	}
	return "t-" + hex.EncodeToString(buf), nil
}
