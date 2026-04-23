import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { useAuth } from "../auth/AuthContext";
import { AuthShell } from "./LoginPage";

// The hCaptcha site key is injected at build time via Vite's env
// plumbing. An unset key disables the widget so local dev / self-
// hosted deploys without a captcha license continue to work; the
// backend treats the missing captchaToken as "captcha disabled"
// when AuthHooks.VerifyCAPTCHA is nil.
const HCAPTCHA_SITEKEY = import.meta.env.VITE_HCAPTCHA_SITEKEY as string | undefined;

declare global {
  interface Window {
    hcaptcha?: {
      render(
        container: HTMLElement,
        opts: { sitekey: string; callback: (token: string) => void; "error-callback"?: () => void },
      ): string | number;
      reset(widgetID?: string | number): void;
    };
  }
}

export function SignupPage() {
  const { signUp } = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [tenantName, setTenantName] = useState("");
  const [captchaToken, setCaptchaToken] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const captchaRef = useRef<HTMLDivElement | null>(null);
  const widgetIdRef = useRef<string | number | null>(null);

  // Lazy-load the hCaptcha script only when a site key is
  // configured. Mounting the script unconditionally would leak a
  // third-party dependency into self-hosted deploys that have
  // deliberately opted out of external CAPTCHA providers.
  useEffect(() => {
    if (!HCAPTCHA_SITEKEY) {
      return;
    }
    const existing = document.querySelector<HTMLScriptElement>(
      "script[data-hcaptcha=\"1\"]",
    );
    if (!existing) {
      const script = document.createElement("script");
      script.src = "https://js.hcaptcha.com/1/api.js";
      script.async = true;
      script.defer = true;
      script.dataset.hcaptcha = "1";
      document.head.appendChild(script);
    }
    const poll = window.setInterval(() => {
      if (!window.hcaptcha || !captchaRef.current || widgetIdRef.current !== null) {
        return;
      }
      widgetIdRef.current = window.hcaptcha.render(captchaRef.current, {
        sitekey: HCAPTCHA_SITEKEY,
        callback: (token) => setCaptchaToken(token),
        "error-callback": () => setCaptchaToken(null),
      });
      window.clearInterval(poll);
    }, 200);
    return () => window.clearInterval(poll);
  }, []);

  const captchaRequired = Boolean(HCAPTCHA_SITEKEY);

  return (
    <AuthShell title="Create a tenant">
      <form
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          setError(null);
          if (captchaRequired && !captchaToken) {
            setError("Please complete the CAPTCHA challenge before continuing.");
            return;
          }
          setSubmitting(true);
          try {
            await signUp({
              email,
              password,
              tenantName,
              captchaToken: captchaToken ?? undefined,
            });
            navigate("/");
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            if (widgetIdRef.current !== null && window.hcaptcha) {
              window.hcaptcha.reset(widgetIdRef.current);
              setCaptchaToken(null);
            }
          } finally {
            setSubmitting(false);
          }
        }}
      >
        <div>
          <label htmlFor="tenantName">Organization</label>
          <input
            id="tenantName"
            value={tenantName}
            onChange={(e) => setTenantName(e.target.value)}
            required
          />
        </div>
        <div>
          <label htmlFor="email">Work email</label>
          <input
            id="email"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
        </div>
        <div>
          <label htmlFor="password">Password</label>
          <input
            id="password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={12}
          />
        </div>
        {captchaRequired && (
          <div>
            <div ref={captchaRef} data-testid="hcaptcha-widget" />
          </div>
        )}
        {error && <div className="danger-text">{error}</div>}
        <button type="submit" disabled={submitting}>
          {submitting ? "Creating tenant…" : "Create tenant"}
        </button>
        <div className="muted" style={{ fontSize: 13 }}>
          Already have an account? <Link to="/login">Sign in</Link>
        </div>
      </form>
    </AuthShell>
  );
}
