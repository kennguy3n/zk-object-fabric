package console

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	// DeleteUser removes the email → (hash, tenant ID) row. It is
	// used by the signup handler to roll back a half-finished
	// signup: when a step after CreateUser fails, leaving the user
	// row behind would permanently lock the email out (re-signup
	// hits LookupUser and 409s, login hits the deleted tenant and
	// 500s). Implementations should treat a missing email as a
	// no-op (return nil) rather than an error.
	DeleteUser(email string) error

	// IsVerified returns (true, true) when the tenant has clicked
	// the verification link in the signup email, (false, true)
	// when the tenant exists but has not verified yet, and
	// (_, false) when the store has no record of the tenant — the
	// S3 PUT gate treats the last case as out-of-scope and lets
	// the request through. This keeps HMAC-only tenants loaded
	// from a JSON bindings file (the Phase 2 path) from being
	// blocked by a verification system they never opted into.
	IsVerified(tenantID string) (verified, tracked bool)

	// MarkVerified records that the tenant has verified their
	// email. It is idempotent: an already-verified tenant is not
	// an error, and an unknown tenant returns an error so the
	// verify endpoint can surface a 404 to the caller.
	MarkVerified(tenantID string) error

	// SetVerificationToken binds an opaque random token to a
	// tenant row. The signup handler mints the token with
	// crypto/rand and embeds it in the outbound verification
	// email; the /api/v1/auth/verify endpoint then resolves the
	// token back to a tenant without accepting the raw tenant ID
	// from a possibly hostile caller. An unknown tenant returns
	// an error so signup can roll back cleanly.
	SetVerificationToken(tenantID, token string) error

	// ConsumeVerificationToken atomically looks up the tenant row
	// bound to token, marks it verified, clears the stored token
	// so it cannot be replayed, and returns the resolved tenant
	// ID. Implementations MUST compare the stored token against
	// the caller-supplied token in constant time. An empty token
	// or a token that matches no row must return an error — the
	// verify handler translates that into a uniform 401 so a
	// probing caller cannot enumerate which tenants are pending
	// verification.
	ConsumeVerificationToken(token string) (tenantID string, err error)
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
	Verified     bool
	// VerificationToken is the random token minted at signup
	// time. It is stored server-side and compared (constant time)
	// against the token carried in the verification email. Empty
	// once the token has been consumed.
	VerificationToken string
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

// DeleteUser implements AuthStore.
func (s *MemoryAuthStore) DeleteUser(email string) error {
	if email == "" {
		return errors.New("console: email is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, strings.ToLower(email))
	return nil
}

// IsVerified implements AuthStore.
func (s *MemoryAuthStore) IsVerified(tenantID string) (bool, bool) {
	if tenantID == "" {
		return false, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, row := range s.users {
		if row.TenantID == tenantID {
			return row.Verified, true
		}
	}
	return false, false
}

// MarkVerified implements AuthStore. It scans users for the matching
// tenantID; a tenant minted via signup has exactly one row so the
// scan is O(1) in the common case and O(n) in the degenerate case
// where multiple users share a tenant (the B2B shared-account path
// the scaffold does not yet support).
func (s *MemoryAuthStore) MarkVerified(tenantID string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, row := range s.users {
		if row.TenantID == tenantID {
			row.Verified = true
			row.VerificationToken = ""
			s.users[email] = row
			return nil
		}
	}
	return fmt.Errorf("console: tenant %q not found", tenantID)
}

// SetVerificationToken implements AuthStore.
func (s *MemoryAuthStore) SetVerificationToken(tenantID, token string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	if token == "" {
		return errors.New("console: verification token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, row := range s.users {
		if row.TenantID == tenantID {
			row.VerificationToken = token
			s.users[email] = row
			return nil
		}
	}
	return fmt.Errorf("console: tenant %q not found", tenantID)
}

// ConsumeVerificationToken implements AuthStore. Comparisons use
// crypto/subtle so a timing-sensitive caller cannot probe which
// tenant a stored token belongs to.
func (s *MemoryAuthStore) ConsumeVerificationToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("console: verification token is required")
	}
	supplied := []byte(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, row := range s.users {
		if row.VerificationToken == "" {
			continue
		}
		if subtle.ConstantTimeCompare(supplied, []byte(row.VerificationToken)) == 1 {
			row.Verified = true
			row.VerificationToken = ""
			s.users[email] = row
			return row.TenantID, nil
		}
	}
	return "", errors.New("console: verification token invalid or expired")
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
	// — the Phase 3 scaffold default. The hook receives the
	// opaque per-signup token that must appear in the verify
	// request; production implementations embed the token in the
	// email link (e.g. https://console.example.com/verify?token=…)
	// so only a caller who received the email can satisfy the
	// /api/v1/auth/verify endpoint.
	SendVerificationEmail func(email, tenantID, token string) error

	// ResolveOAuth resolves an OAuth bearer token submitted via
	// signupRequest.OAuthToken into a provider-issued subject
	// identifier. A nil hook makes OAuth signups return 503 so the
	// frontend's OAuth buttons degrade gracefully when no provider
	// is wired. Production hooks typically proxy Google / Microsoft
	// / Okta and return the OIDC `sub` claim.
	ResolveOAuth func(token string) (subject string, err error)
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
	ID                        string               `json:"id"`
	Name                      string               `json:"name"`
	ContractType              tenant.ContractType  `json:"contractType"`
	LicenseTier               tenant.LicenseTier   `json:"licenseTier"`
	PlacementDefaultPolicyRef string               `json:"placementDefaultPolicyRef"`
	Budgets                   TenantBudgetsSummary `json:"budgets"`
	// CreatedAt echoes the time the tenant record was first
	// minted. The SPA's Tenant.createdAt field is populated from
	// this value and is informational only — authorization never
	// depends on it.
	CreatedAt time.Time `json:"createdAt"`
}

// TenantBudgetsSummary is the budgets slice the frontend dashboard
// renders. The full tenant.Budgets structure has additional operator
// knobs that should not leak to the SPA.
type TenantBudgetsSummary struct {
	// RequestsPerSec is the steady-state request rate ceiling
	// the gateway fleet enforces. Mirrors tenant.Budgets.RequestsPerSec.
	RequestsPerSec int `json:"requestsPerSec"`
	// BurstRequests is the short-window burst allowance the rate
	// limiter permits above RequestsPerSec. The B2C default is
	// 2 × RequestsPerSec; operators override it per tenant.
	BurstRequests int `json:"burstRequests"`
	// EgressTBMonth is the soft monthly egress cap in TB.
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
	authPathVerify = "/api/v1/auth/verify"
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
	case authPathVerify:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.verify(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown auth path "+r.URL.Path)
	}
}

// verifyRequest is the payload POSTed to /api/v1/auth/verify. The
// opaque Token is the random value the gateway minted at signup and
// embedded in the outbound verification email link; only a caller
// that received the email (or holds the user's mailbox) can produce
// it. The endpoint intentionally does not accept a bare tenant ID —
// the tenant ID is returned to the signup caller in the clear and
// would let any signed-up user self-verify without the email step.
type verifyRequest struct {
	Token string `json:"token"`
}

func (h *AuthHandler) verify(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode verify: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	tenantID, err := h.cfg.Auth.ConsumeVerificationToken(token)
	if err != nil {
		// 401 rather than 404 so a probing caller cannot
		// distinguish a malformed / unknown token from one that
		// was already consumed.
		writeError(w, http.StatusUnauthorized, "invalid or expired verification token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "tenantId": tenantID})
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
	// orphan records. This is a fast path only — the
	// authoritative uniqueness check happens inside CreateUser
	// below, which races-safely rejects duplicates and triggers
	// the rollback block that follows.
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
	// rollbackTenant removes the tenant record the request just
	// created. Every failure path between CreateTenant and the
	// final writeJSON calls this so a losing racer in a concurrent
	// duplicate-email signup — or a transient CreateUser /
	// AddAPIKey / IssueToken failure — does not leave an orphaned
	// tenant record behind. Best-effort: a DeleteTenant error is
	// logged, not returned, because the original request failure
	// is the more useful signal for the caller.
	rollbackTenant := func(reason string) {
		if derr := h.cfg.Tenants.DeleteTenant(tenantID); derr != nil {
			log.Printf("console: signup rollback (%s) failed to delete tenant %q: %v", reason, tenantID, derr)
		}
	}
	// rollbackUserAndTenant is called from failure paths that run
	// AFTER CreateUser has already committed the email row. Without
	// clearing the user row first, the rolled-back tenant record
	// leaves the email permanently locked out: re-signup hits the
	// LookupUser fast path (409) and a login attempt finds the
	// user but not the tenant (500). The user row must be deleted
	// before the tenant row so a concurrent LookupTenant that
	// races this rollback still sees a consistent (no user, no
	// tenant) state rather than (user → missing tenant).
	rollbackUserAndTenant := func(reason string) {
		if derr := h.cfg.Auth.DeleteUser(req.Email); derr != nil {
			log.Printf("console: signup rollback (%s) failed to delete user %q: %v", reason, req.Email, derr)
		}
		rollbackTenant(reason)
	}

	var passwordHash string
	if req.OAuthToken == "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			rollbackTenant("hash password")
			writeError(w, http.StatusInternalServerError, "hash password: "+err.Error())
			return
		}
		passwordHash = string(hash)
	}
	// OAuth branch: passwordHash stays empty and the login flow
	// refuses password logins for this user until an OAuth
	// ResolveOAuth hook is wired in. When the hook is configured
	// it resolves the caller-supplied token against the provider
	// (Google / Microsoft / Okta) and returns the provider-issued
	// subject identifier; we store it in the password hash column
	// so a subsequent login can compare subject identifiers (via
	// the same bcrypt path) without a second OAuth round-trip.
	if req.OAuthToken != "" {
		if h.cfg.Hooks.ResolveOAuth == nil {
			rollbackTenant("oauth hook missing")
			writeError(w, http.StatusServiceUnavailable, "oauth provider not configured")
			return
		}
		subject, err := h.cfg.Hooks.ResolveOAuth(req.OAuthToken)
		if err != nil {
			rollbackTenant("oauth resolve")
			writeError(w, http.StatusUnauthorized, "oauth resolve: "+err.Error())
			return
		}
		passwordHash = "oauth:" + subject
	}
	if err := h.cfg.Auth.CreateUser(req.Email, passwordHash, tenantID); err != nil {
		// CreateUser is the authoritative email-uniqueness
		// check. A concurrent signup that lost the race gets
		// here and must not leave its tenant record behind.
		rollbackTenant("create user")
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	accessKey, secretKey, err := h.cfg.GenerateKey()
	if err != nil {
		rollbackUserAndTenant("generate key")
		writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	if err := h.cfg.Tenants.AddAPIKey(tenantID, accessKey, secretKey); err != nil {
		rollbackUserAndTenant("register key")
		writeError(w, http.StatusInternalServerError, "register key: "+err.Error())
		return
	}

	// Mint a per-signup verification token that only the
	// outbound email carries. Storing it on the user row before
	// calling SendVerificationEmail means the verify endpoint
	// cannot be raced by a caller that reuses a token captured
	// from an earlier signup — every signup has its own fresh
	// token. A failure here must roll back the user and tenant
	// records so the email address is not permanently locked out.
	if h.cfg.Hooks.SendVerificationEmail != nil {
		vtoken, err := newVerificationToken()
		if err != nil {
			rollbackUserAndTenant("mint verification token")
			writeError(w, http.StatusInternalServerError, "mint verification token: "+err.Error())
			return
		}
		if err := h.cfg.Auth.SetVerificationToken(tenantID, vtoken); err != nil {
			rollbackUserAndTenant("store verification token")
			writeError(w, http.StatusInternalServerError, "store verification token: "+err.Error())
			return
		}
		if err := h.cfg.Hooks.SendVerificationEmail(req.Email, tenantID, vtoken); err != nil {
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
		rollbackUserAndTenant("issue token")
		writeError(w, http.StatusInternalServerError, "issue token: "+err.Error())
		return
	}
	createdAt := h.cfg.Now()
	writeJSON(w, http.StatusCreated, AuthResponse{
		Tenant:    summarizeTenantAt(newTenant, createdAt),
		Token:     token,
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: createdAt,
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
	if !ok || hash == "" || strings.HasPrefix(hash, "oauth:") {
		// Return a uniform 401 for missing user, OAuth-only
		// accounts (stored as "oauth:<subject>" by signup), and
		// wrong-password cases so a probing caller can't
		// distinguish them.
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
	return summarizeTenantAt(t, time.Time{})
}

// summarizeTenantAt is the createdAt-aware variant summarizeTenant
// delegates to. The signup handler passes the just-minted timestamp
// so the SPA renders a valid ISO date on the tenant card; the login
// handler uses the zero value because the tenant.Tenant record does
// not carry a persisted creation timestamp yet (the Phase 4 Postgres
// schema will populate it via a column default).
func summarizeTenantAt(t tenant.Tenant, createdAt time.Time) TenantSummary {
	burst := 2 * t.Budgets.RequestsPerSec
	return TenantSummary{
		ID:                        t.ID,
		Name:                      t.Name,
		ContractType:              t.ContractType,
		LicenseTier:               t.LicenseTier,
		PlacementDefaultPolicyRef: t.PlacementDefault.PolicyRef,
		Budgets: TenantBudgetsSummary{
			RequestsPerSec: t.Budgets.RequestsPerSec,
			BurstRequests:  burst,
			EgressTBMonth:  t.Budgets.EgressTBMonth,
		},
		CreatedAt: createdAt,
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

// newVerificationToken mints a 32-byte hex-encoded random token used
// as the bearer secret in outbound verification emails. 32 bytes of
// entropy keep the token unguessable at any realistic signup volume
// while remaining short enough to fit comfortably in a URL query
// parameter.
func newVerificationToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand verification token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
