import { api } from "./client";
import type { Tenant } from "./types";

// auth.ts is the frontend-side mirror of api/console/auth_handler.go.
// It keeps signup / login wire shapes in one place so the auth
// context, login page, and signup page can share them without
// importing ApiClient directly.
//
// The gateway mounts the auth endpoints at /api/v1/auth/signup and
// /api/v1/auth/login; the shared ApiClient in src/api/client.ts is
// already rooted at /api/v1 so the calls below pass relative paths.

export interface SignupInput {
  email: string;
  password: string;
  tenantName: string;
  /**
   * Optional CAPTCHA token returned by the production CAPTCHA widget.
   * The backend is currently a no-op; operators wire this into their
   * CAPTCHA provider via console.AuthHooks.VerifyCAPTCHA when the
   * scaffold graduates to production.
   */
  captchaToken?: string;
  /**
   * Optional OAuth provider token (e.g. Google ID token). When
   * present the password field may be left blank and the backend
   * resolves the token against the configured OAuth provider. The
   * Phase 3 scaffold accepts the field but does not wire an OAuth
   * provider yet — see AuthHooks in api/console/auth_handler.go.
   */
  oauthToken?: string;
}

export interface LoginInput {
  email: string;
  password: string;
}

export interface AuthResponse {
  tenant: Tenant;
  token: string;
  /**
   * accessKey / secretKey are only returned on signup — the backend
   * never re-reveals the S3 secret on subsequent logins, matching
   * the one-time reveal the console enforces on API-key create.
   */
  accessKey?: string;
  secretKey?: string;
  createdAt?: string;
}

export async function login(input: LoginInput): Promise<AuthResponse> {
  return api.login(input.email, input.password);
}

export async function signup(input: SignupInput): Promise<AuthResponse> {
  return api.signup(input);
}
