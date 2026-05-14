package handlers

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// SuccessConfig holds the dependencies for the OAuth2 callback / success handler.
type SuccessConfig struct {
	CognitoClient *auth.CognitoClient
	AuthService   *service.AuthService
	CognitoDomain string
}

var successTmpl = template.Must(template.New("success").Parse(`<!DOCTYPE html>
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
      background: rgba(30, 41, 59, 0.8);
      border: 1px solid rgba(148, 163, 184, 0.15);
      border-radius: 16px;
      padding: 3rem 2.5rem;
      max-width: 580px;
      width: 90%;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5);
    }
    .card-center { text-align: center; }
    .icon { font-size: 3.5rem; margin-bottom: 1rem; }
    h1 { font-size: 1.75rem; font-weight: 700; margin-bottom: 0.5rem; color: #f8fafc; }
    h2 { font-size: 1.15rem; font-weight: 600; margin: 1.5rem 0 0.75rem; color: #cbd5e1; }
    p { font-size: 1rem; line-height: 1.6; color: #94a3b8; margin-bottom: 1rem; }
    .btn {
      display: inline-block; padding: 0.75rem 2rem; font-size: 1rem; font-weight: 600;
      color: #fff; background: linear-gradient(135deg, #6366f1, #8b5cf6);
      border-radius: 8px; text-decoration: none; transition: opacity 0.2s, transform 0.2s;
    }
    .btn:hover { opacity: 0.9; transform: translateY(-1px); }
    .note { margin-top: 1.5rem; font-size: 0.85rem; color: #64748b; }
    code {
      background: rgba(148, 163, 184, 0.12); padding: 0.15em 0.4em;
      border-radius: 4px; font-size: 0.9em;
    }
    .spinner {
      display: inline-block; width: 28px; height: 28px;
      border: 3px solid rgba(148,163,184,0.2); border-top-color: #8b5cf6;
      border-radius: 50%; animation: spin 0.8s linear infinite;
      vertical-align: middle; margin-right: 0.5rem;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    .detail-row {
      display: flex; align-items: center; justify-content: space-between;
      background: rgba(15, 23, 42, 0.6); border: 1px solid rgba(148, 163, 184, 0.1);
      border-radius: 8px; padding: 0.6rem 0.75rem; margin-bottom: 0.5rem;
      font-size: 0.88rem;
    }
    .detail-label { color: #64748b; min-width: 110px; flex-shrink: 0; }
    .detail-value {
      color: #e2e8f0; word-break: break-all; flex: 1; margin: 0 0.5rem;
      font-family: "SF Mono", "Fira Code", monospace;
    }
    .copy-btn {
      background: none; border: 1px solid rgba(148,163,184,0.2); border-radius: 4px;
      color: #94a3b8; cursor: pointer; padding: 0.25rem 0.5rem; font-size: 0.75rem;
      transition: color 0.2s, border-color 0.2s; flex-shrink: 0;
    }
    .copy-btn:hover { color: #e2e8f0; border-color: #8b5cf6; }
    #provision-spinner { margin-top: 1.5rem; }
    #provision-details { display: none; }
    #provision-error { display: none; color: #f87171; margin-top: 1rem; font-size: 0.9rem; }
  </style>
</head>
<body>
  <div class="card">
    <div class="card-center">
      <div class="icon">&#10003;</div>
      <h1>You're All Set!</h1>
      <p>Your account has been authenticated successfully.</p>
    </div>

    <div id="provision-spinner" class="card-center">
      <div class="spinner"></div>
      <span style="color:#94a3b8">Setting up your default datapack&hellip;</span>
    </div>

    <div id="provision-error" class="card-center"></div>

    <div id="provision-details">
      <h2>Your Default Datapack</h2>
      <div class="detail-row">
        <span class="detail-label">Datapack</span>
        <span class="detail-value" id="dp-name"></span>
        <button class="copy-btn" onclick="copyVal('dp-name')">Copy</button>
      </div>
      <h2>MCP Server</h2>
      <div class="detail-row">
        <span class="detail-label">URL</span>
        <span class="detail-value" id="mcp-url"></span>
        <button class="copy-btn" onclick="copyVal('mcp-url')">Copy</button>
      </div>
      <div class="detail-row">
        <span class="detail-label">API Key</span>
        <span class="detail-value" id="mcp-key"></span>
        <button class="copy-btn" onclick="copyVal('mcp-key')">Copy</button>
      </div>
      <h2>Database Access</h2>
      <div class="detail-row">
        <span class="detail-label">Username</span>
        <span class="detail-value" id="db-user"></span>
        <button class="copy-btn" onclick="copyVal('db-user')">Copy</button>
      </div>
      <div class="detail-row">
        <span class="detail-label">Password</span>
        <span class="detail-value" id="db-pass"></span>
        <button class="copy-btn" onclick="copyVal('db-pass')">Copy</button>
      </div>
      <div class="detail-row">
        <span class="detail-label">Conn String</span>
        <span class="detail-value" id="db-conn"></span>
        <button class="copy-btn" onclick="copyVal('db-conn')">Copy</button>
      </div>

      <div class="card-center" style="margin-top:1.5rem">
        <a class="btn" href="https://github.com/yugabyte/meko/releases" target="_blank" rel="noopener">
          Download Meko CLI
        </a>
        <p class="note">
          Or clone the repo and build from source:<br>
          <code>git clone https://github.com/yugabyte/meko.git</code>
        </p>
      </div>
    </div>
  </div>

  <script>
  function copyVal(id) {
    var el = document.getElementById(id);
    if (!el) return;
    navigator.clipboard.writeText(el.textContent).then(function() {
      var btn = el.parentElement.querySelector('.copy-btn');
      if (btn) { btn.textContent = 'Copied!'; setTimeout(function(){ btn.textContent = 'Copy'; }, 1500); }
    });
  }

  (function() {
    var token = {{.AccessToken}};
    if (!token) return;

    var spinner  = document.getElementById('provision-spinner');
    var details  = document.getElementById('provision-details');
    var errorDiv = document.getElementById('provision-error');
    var attempts = 0;
    var maxAttempts = 90;

    function poll() {
      attempts++;
      fetch('/api/v1/internal/provision-status', {
        headers: { 'Authorization': 'Bearer ' + token }
      }).then(function(r) {
        if (r.ok) return r.json();
        if (r.status === 404 && attempts < maxAttempts) {
          setTimeout(poll, 2000);
          return null;
        }
        throw new Error('status ' + r.status);
      }).then(function(body) {
        if (!body) return;
        var d = body.data;
        document.getElementById('dp-name').textContent  = d.datapack_name;
        document.getElementById('mcp-url').textContent   = d.mcp_server_url;
        document.getElementById('mcp-key').textContent   = d.mcp_api_key;
        document.getElementById('db-user').textContent   = d.db_user_name;
        document.getElementById('db-pass').textContent   = d.db_password;
        document.getElementById('db-conn').textContent   = d.db_conn_str;
        spinner.style.display  = 'none';
        details.style.display  = 'block';
      }).catch(function(err) {
        spinner.style.display  = 'none';
        errorDiv.style.display = 'block';
        errorDiv.textContent   = 'Provisioning failed. Please contact support. (' + err.message + ')';
      });
    }

    poll();
  })();
  </script>
</body>
</html>`))

const staticSuccessHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Welcome to Meko</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      min-height: 100vh; display: flex; align-items: center; justify-content: center;
      background: linear-gradient(135deg, #0f172a 0%, #1e293b 100%); color: #e2e8f0;
    }
    .card {
      text-align: center; background: rgba(30, 41, 59, 0.8);
      border: 1px solid rgba(148, 163, 184, 0.15); border-radius: 16px;
      padding: 3rem 2.5rem; max-width: 480px; width: 90%;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5);
    }
    .icon { font-size: 3.5rem; margin-bottom: 1rem; }
    h1 { font-size: 1.75rem; font-weight: 700; margin-bottom: 0.5rem; color: #f8fafc; }
    p { font-size: 1rem; line-height: 1.6; color: #94a3b8; margin-bottom: 1.5rem; }
    .btn {
      display: inline-block; padding: 0.75rem 2rem; font-size: 1rem; font-weight: 600;
      color: #fff; background: linear-gradient(135deg, #6366f1, #8b5cf6);
      border-radius: 8px; text-decoration: none; transition: opacity 0.2s, transform 0.2s;
    }
    .btn:hover { opacity: 0.9; transform: translateY(-1px); }
    .note { margin-top: 1.5rem; font-size: 0.85rem; color: #64748b; }
    code { background: rgba(148, 163, 184, 0.12); padding: 0.15em 0.4em; border-radius: 4px; font-size: 0.9em; }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">&#10003;</div>
    <h1>You're All Set!</h1>
    <p>Your account has been authenticated successfully. Download the <strong>Meko CLI</strong> to start managing your datapacks.</p>
    <a class="btn" href="https://github.com/yugabyte/meko/releases" target="_blank" rel="noopener">Download Meko CLI</a>
    <p class="note">Or clone the repo and build from source:<br><code>git clone https://github.com/yugabyte/meko.git</code></p>
  </div>
</body>
</html>`

// SuccessHandler handles the Cognito Hosted UI redirect. When a ?code=
// query parameter is present, it exchanges the authorization code for
// tokens, creates a local account (JIT provisioning), and renders the
// provisioning page that polls for datapack details. Without a code it
// renders a static welcome page.
func SuccessHandler(cfg *SuccessConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		code := c.QueryParam("code")
		if code == "" || cfg == nil {
			return c.HTML(http.StatusOK, staticSuccessHTML)
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
			return c.HTML(http.StatusOK, staticSuccessHTML)
		}

		cfg.AuthService.EnsureAccountFromIDToken(
			c.Request().Context(), result.IDToken,
		)

		return renderSuccessWithToken(c, result.AccessToken)
	}
}

// renderSuccessWithToken executes the provisioning template with the
// access token embedded for JS polling.
func renderSuccessWithToken(c echo.Context, accessToken string) error {
	var buf bytes.Buffer
	if err := successTmpl.Execute(&buf, map[string]string{
		"AccessToken": accessToken,
	}); err != nil {
		slog.Error("failed to render success template", "error", err)
		return c.HTML(http.StatusOK, staticSuccessHTML)
	}
	return c.HTML(http.StatusOK, buf.String())
}

// DatapackProvisionStatusHandler returns credentials for the datapack
// in path scope (/datapacks/:name). Backs the per-datapack Connect
// dialog. Access is enforced upstream by RequireDatapackAccess (owner or
// accepted share with agent_access).
func DatapackProvisionStatusHandler(dpRepo *repository.DatapackRepo) echo.HandlerFunc {
	return func(c echo.Context) error {
		dp, err := auth.GetDatapack(c)
		if err != nil {
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "datapack context missing")
		}

		details, err := dpRepo.GetDatapackProvisionDetails(dp.ID)
		if err != nil {
			slog.Error("datapack provision-status query failed",
				"datapack_id", dp.ID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to fetch datapack provisioning details")
		}
		if details == nil {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "datapack no longer exists")
		}

		// We need the caller's auth token to populate MCPAPIKey as its used
		// by the WorkbenchLM to connect to the MCP server.
		token, err := auth.ExtractBearerToken(c.Request())
		if err != nil {
			return apiError(c, http.StatusUnauthorized,
				"UNAUTHORIZED", "unable to extract access token")
		}
		details.MCPAPIKey = token

		return ok(c, http.StatusOK, details)
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
