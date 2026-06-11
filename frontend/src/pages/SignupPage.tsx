import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { useAuth } from "../auth/AuthContext";
import { InlineError } from "../components/states";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
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
    <AuthShell title="Create a tenant" subtitle="Spin up a zero-knowledge object store in seconds.">
      <form
        className="space-y-4"
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
        <div className="space-y-1.5">
          <Label htmlFor="tenantName">Organization</Label>
          <Input
            id="tenantName"
            value={tenantName}
            onChange={(e) => setTenantName(e.target.value)}
            required
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="email">Work email</Label>
          <Input
            id="email"
            type="email"
            autoComplete="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="password">Password</Label>
          <Input
            id="password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={12}
          />
          <p className="text-xs text-muted-foreground">Minimum 12 characters.</p>
        </div>
        {captchaRequired && <div ref={captchaRef} data-testid="hcaptcha-widget" />}
        {error && <InlineError message={error} />}
        <Button type="submit" disabled={submitting} className="w-full">
          {submitting ? "Creating tenant…" : "Create tenant"}
        </Button>
        <p className="text-center text-sm text-muted-foreground">
          Already have an account?{" "}
          <Link to="/login" className="font-medium text-primary hover:underline">
            Sign in
          </Link>
        </p>
      </form>
    </AuthShell>
  );
}
