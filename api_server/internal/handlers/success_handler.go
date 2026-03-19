package handlers

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// SuccessConfig holds the dependencies for the OAuth2 callback / success handler.
type SuccessConfig struct {
	CognitoClient *auth.CognitoClient
	AuthService   *service.AuthService
	CognitoDomain string
}

const successHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Welcome to Meko</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      background: linear-gradient(135deg, #0f172a 0%, #1e293b 100%);
      color: #e2e8f0;
    }
    .card {
      text-align: center;
      background: rgba(30, 41, 59, 0.8);
      border: 1px solid rgba(148, 163, 184, 0.15);
      border-radius: 16px;
      padding: 3rem 2.5rem;
      max-width: 480px;
      width: 90%;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5);
    }
    .icon {
      font-size: 3.5rem;
      margin-bottom: 1rem;
    }
    h1 {
      font-size: 1.75rem;
      font-weight: 700;
      margin-bottom: 0.5rem;
      color: #f8fafc;
    }
    p {
      font-size: 1rem;
      line-height: 1.6;
      color: #94a3b8;
      margin-bottom: 1.5rem;
    }
    .btn {
      display: inline-block;
      padding: 0.75rem 2rem;
      font-size: 1rem;
      font-weight: 600;
      color: #fff;
      background: linear-gradient(135deg, #6366f1, #8b5cf6);
      border-radius: 8px;
      text-decoration: none;
      transition: opacity 0.2s, transform 0.2s;
    }
    .btn:hover { opacity: 0.9; transform: translateY(-1px); }
    .note {
      margin-top: 1.5rem;
      font-size: 0.85rem;
      color: #64748b;
    }
    code {
      background: rgba(148, 163, 184, 0.12);
      padding: 0.15em 0.4em;
      border-radius: 4px;
      font-size: 0.9em;
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">&#10003;</div>
    <h1>You're All Set!</h1>
    <p>Your account has been authenticated successfully. Download the <strong>Meko CLI</strong> to start managing your datapacks.</p>
    <a class="btn" href="https://github.com/yugabyte/meko/releases" target="_blank" rel="noopener">
      Download Meko CLI
    </a>
    <p class="note">
      Or clone the repo and build from source:<br>
      <code>git clone https://github.com/yugabyte/meko.git</code>
    </p>
  </div>
</body>
</html>`

// SuccessHandler handles the Cognito Hosted UI redirect. When a ?code=
// query parameter is present, it exchanges the authorization code for
// tokens and creates a local account (JIT provisioning). It always
// renders the success HTML page.
//
// If cfg is nil (auth disabled), it renders the static page without
// code exchange.
func SuccessHandler(cfg *SuccessConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		code := c.QueryParam("code")
		if code == "" || cfg == nil {
			return c.HTML(http.StatusOK, successHTML)
		}

		redirectURI := currentRedirectURI(c)

		result, err := cfg.CognitoClient.ExchangeCode(
			c.Request().Context(),
			code, cfg.CognitoDomain, redirectURI,
		)
		if err != nil {
			slog.Error("oauth2 code exchange failed",
				"error", err,
			)
			return c.HTML(http.StatusOK, successHTML)
		}

		cfg.AuthService.EnsureAccountFromIDToken(
			c.Request().Context(), result.IDToken,
		)

		return c.HTML(http.StatusOK, successHTML)
	}
}

// currentRedirectURI reconstructs the redirect URI from the incoming
// request (scheme + host + path, without query params) so it matches
// what Cognito expects.
func currentRedirectURI(c echo.Context) string {
	scheme := c.Scheme()
	if fwd := c.Request().Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	}
	return scheme + "://" + c.Request().Host + c.Path()
}
