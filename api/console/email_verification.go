package console

import (
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/url"
	"os"
	"strings"
)

// SESEmailConfig configures the SES-over-SMTP verification-email
// sender. AWS SES exposes both a REST API (sesv2) and a standard
// SMTP interface; we use the SMTP path so the gateway keeps its
// current dependency surface (net/smtp is stdlib) instead of pulling
// in the sesv2 SDK module. Any SMTP-capable transactional email
// provider — Postmark, Mailgun, Resend, or raw Postfix — also works
// against this same type.
//
// FromAddress is the envelope sender and appears in the From: header
// (required). Region is informational and flows into the default
// SES SMTP host (email-smtp.<region>.amazonaws.com) when SMTPHost
// is empty. SMTPHost / SMTPPort / SMTPUser / SMTPPassword fall back
// to AWS_SES_SMTP_HOST / _PORT / _USER / _PASSWORD environment
// variables so operators can rotate credentials without editing
// config. VerifyBaseURL is the user-facing URL the verification link
// points at; the sender appends ?token=<opaque> in a future
// phase — for now the message carries a stubbed link so the end-to-
// end path (CAPTCHA → signup → email dispatch) is testable.
type SESEmailConfig struct {
	FromAddress   string
	Region        string
	SMTPHost      string
	SMTPPort      string
	SMTPUser      string
	SMTPPassword  string
	VerifyBaseURL string
}

// smtpSender is the interface the real smtp.SendMail signature
// satisfies. Declared so tests can swap it out without reaching
// into net.
type smtpSender func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

// NewSESEmailSender returns an AuthHooks.SendVerificationEmail
// callback that dispatches a verification email via SES SMTP (or any
// provider exposing the same interface).
//
// The returned closure is best-effort: transient SMTP errors surface
// as (non-nil) return values, but the signup handler already treats
// email-send failures as non-fatal — see auth_handler.go §signup —
// so the user still gets a tenant and the operator can replay
// delivery from a background retry queue.
func NewSESEmailSender(cfg SESEmailConfig) (func(email, tenantID, token string) error, error) {
	return newSESEmailSenderWithSMTP(cfg, smtp.SendMail)
}

func newSESEmailSenderWithSMTP(cfg SESEmailConfig, send smtpSender) (func(email, tenantID, token string) error, error) {
	from := strings.TrimSpace(cfg.FromAddress)
	if from == "" {
		return nil, errors.New("ses: FromAddress is required")
	}
	host := cfg.SMTPHost
	if host == "" {
		host = os.Getenv("AWS_SES_SMTP_HOST")
	}
	if host == "" && cfg.Region != "" {
		host = fmt.Sprintf("email-smtp.%s.amazonaws.com", cfg.Region)
	}
	if host == "" {
		return nil, errors.New("ses: SMTP host is required (set SMTPHost, Region, or AWS_SES_SMTP_HOST)")
	}
	port := cfg.SMTPPort
	if port == "" {
		port = os.Getenv("AWS_SES_SMTP_PORT")
	}
	if port == "" {
		port = "587"
	}
	user := cfg.SMTPUser
	if user == "" {
		user = os.Getenv("AWS_SES_SMTP_USER")
	}
	pass := cfg.SMTPPassword
	if pass == "" {
		pass = os.Getenv("AWS_SES_SMTP_PASSWORD")
	}
	verifyBase := cfg.VerifyBaseURL
	if verifyBase == "" {
		verifyBase = "https://console.example.com/verify"
	}

	addr := net.JoinHostPort(host, port)
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}

	return func(email, tenantID, token string) error {
		to := strings.TrimSpace(email)
		if to == "" {
			return errors.New("ses: recipient email is required")
		}
		if strings.TrimSpace(token) == "" {
			// A missing token would render the email useless —
			// the receiving user could not satisfy the verify
			// endpoint. Fail loudly so the signup rollback fires
			// instead of shipping a broken link.
			return errors.New("ses: verification token is required")
		}
		verifyURL := verifyBase +
			"?tenant=" + url.QueryEscape(tenantID) +
			"&token=" + url.QueryEscape(token)
		msg := buildVerificationMessage(from, to, tenantID, verifyURL)
		return send(addr, auth, from, []string{to}, msg)
	}, nil
}

// buildVerificationMessage renders a minimal text/plain RFC 5322
// message. The sender intentionally omits HTML and tracking pixels:
// the verification link is the only payload the user needs.
func buildVerificationMessage(from, to, tenantID, verifyURL string) []byte {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(from)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(to)
	b.WriteString("\r\n")
	b.WriteString("Subject: Verify your zk-object-fabric tenant\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString("Welcome to zk-object-fabric.\r\n\r\n")
	b.WriteString("Tenant ID: ")
	b.WriteString(tenantID)
	b.WriteString("\r\n\r\n")
	b.WriteString("Click the link below to verify your email and unlock your first upload:\r\n")
	b.WriteString(verifyURL)
	b.WriteString("\r\n\r\n")
	b.WriteString("If you didn't sign up, you can ignore this message.\r\n")
	return []byte(b.String())
}
