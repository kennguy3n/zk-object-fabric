package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HCaptchaVerifyURL is the hCaptcha siteverify endpoint. Declared as
// a var (rather than a const) so tests can swap it with a local
// fake; production deploys must leave it pointing at hCaptcha.
var HCaptchaVerifyURL = "https://hcaptcha.com/siteverify"

// hcaptchaResponse mirrors the documented siteverify response shape.
// Only the subset of fields the Phase 3 gate consumes is declared;
// unused fields (challenge_ts, hostname, credit) are intentionally
// omitted so a forward-compatible API change to hCaptcha never fails
// our decode.
type hcaptchaResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes,omitempty"`
}

// hcaptchaHTTPDoer narrows http.Client to the one method the
// verifier uses. Tests inject a fake here instead of stubbing the
// full net/http.RoundTripper surface.
type hcaptchaHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewHCaptchaVerifier returns an AuthHooks.VerifyCAPTCHA callback
// that POSTs (secret, token) to the hCaptcha siteverify API and
// rejects the signup when the response is not {"success": true}.
//
// secret is the hCaptcha site secret (never the site key). An empty
// secret returns a verifier that always rejects — the misconfigured
// path is strictly safer than the accept-by-default alternative.
//
// siteverifyURL is optional; when empty the public hCaptcha URL is
// used. Tests use this override to point at a local stub.
func NewHCaptchaVerifier(secret, siteverifyURL string) func(token string) error {
	return newHCaptchaVerifierWithClient(secret, siteverifyURL, &http.Client{Timeout: 10 * time.Second})
}

func newHCaptchaVerifierWithClient(secret, siteverifyURL string, client hcaptchaHTTPDoer) func(token string) error {
	endpoint := siteverifyURL
	if endpoint == "" {
		endpoint = HCaptchaVerifyURL
	}
	return func(token string) error {
		if secret == "" {
			return errors.New("hcaptcha: site secret is not configured")
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return errors.New("hcaptcha: captcha token is required")
		}
		form := url.Values{}
		form.Set("secret", secret)
		form.Set("response", token)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return fmt.Errorf("hcaptcha: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("hcaptcha: siteverify: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("hcaptcha: siteverify returned %d", resp.StatusCode)
		}
		var body hcaptchaResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return fmt.Errorf("hcaptcha: decode siteverify response: %w", err)
		}
		if !body.Success {
			if len(body.ErrorCodes) > 0 {
				return fmt.Errorf("hcaptcha: rejected: %s", strings.Join(body.ErrorCodes, ","))
			}
			return errors.New("hcaptcha: rejected")
		}
		return nil
	}
}
