package functional

import (
	"fmt"
	"net/http"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/auth_login_flow",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "On a stack with the Cognito mock: sign up, refuse a login before the email is confirmed and a wrong confirmation code, confirm, log in, and read the account back from /auth/me. " +
			"A wrong password and an unknown user are both 401. An invited user (first login with a temporary password) gets the new-password challenge, completes it and is logged in. " +
			"Tokens the pool did not issue are refused with 401: a missing one, garbage, one from another pool, an expired one, an ID token, a payload changed under its signature, one signed by another key, and a PAT on a route that takes JWTs only. " +
			"The product has no token refresh route, so none is tested.",
		Run: authLoginFlow,
	})
}

const newPassword = "Mekoqa-new-pass-1!"

func authLoginFlow(t *core.T) error {
	anon := anonymousAPI(t)
	ctl := controlFor(t)
	suffix := t.ResourceSuffix()
	email := "signup-" + suffix + "@mekoqa.test"
	password := "Mekoqa-signup-1!"
	var accountID string

	if err := t.Step("signup_and_refuse_login_until_confirmed", func(h *core.StepHandle) error {
		weak, err := anon.Raw(t, http.MethodPost, "/auth/signup", mekoapi.SignupRequest{Email: email, Password: "weak", FirstName: "Sue", LastName: "Nup"})
		if err != nil {
			return err
		}
		if err := wantStatus("signup with a weak password", weak, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
			return err
		}
		res, err := anon.Signup(t, mekoapi.SignupRequest{Email: email, Password: password, FirstName: "Sue", LastName: "Nup"})
		if err != nil {
			return err
		}
		accountID = res.AccountID
		h.Set("account_id", accountID)
		if accountID == "" || res.Email != email {
			return fmt.Errorf("signup answered %+v", res)
		}
		again, err := anon.Raw(t, http.MethodPost, "/auth/signup", mekoapi.SignupRequest{Email: email, Password: password, FirstName: "Sue", LastName: "Nup"})
		if err != nil {
			return err
		}
		if err := wantStatus("signing up the same email twice", again, http.StatusConflict, "CONFLICT"); err != nil {
			return err
		}
		early, err := anon.Raw(t, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password})
		if err != nil {
			return err
		}
		return wantStatus("login before confirming", early, http.StatusForbidden, "USER_NOT_CONFIRMED")
	}); err != nil {
		return err
	}

	var session *mekoapi.Client
	if err := t.Step("confirm_and_log_in", func(h *core.StepHandle) error {
		bad, err := anon.Raw(t, http.MethodPost, "/auth/confirm-signup", map[string]string{"email": email, "code": "000000"})
		if err != nil {
			return err
		}
		if err := wantStatus("a wrong confirmation code", bad, http.StatusBadRequest, "INVALID_CODE"); err != nil {
			return err
		}
		if err := anon.ConfirmSignup(t, email, cognitomock.ConfirmationCode); err != nil {
			return err
		}
		login, err := anon.Login(t, email, password)
		if err != nil {
			return err
		}
		if login.AccessToken == "" || login.IDToken == "" || login.RefreshToken == "" || login.ExpiresIn <= 0 {
			return fmt.Errorf("login answered %+v, want an access, ID and refresh token and an expiry", login)
		}
		session = anon.WithToken(login.AccessToken)
		me, err := session.Me(t)
		if err != nil {
			return err
		}
		h.Set("user_type", me.UserType)
		if me.AccountID != accountID || me.Email != email || me.FirstName != "Sue" || me.LastName != "Nup" || !me.EmailVerified {
			return fmt.Errorf("/auth/me answered %+v, want account %s, %s, Sue Nup, email verified", me, accountID, email)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("bad_credentials_are_401", func(*core.StepHandle) error {
		for what, body := range map[string]map[string]string{
			"a wrong password": {"email": email, "password": "Not-the-pass-1!"},
			"an unknown user":  {"email": "nobody-" + suffix + "@mekoqa.test", "password": password},
		} {
			got, err := anon.Raw(t, http.MethodPost, "/auth/login", body)
			if err != nil {
				return err
			}
			if err := wantStatus("login with "+what, got, http.StatusUnauthorized, "UNAUTHORIZED"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("invited_user_sets_a_password", func(h *core.StepHandle) error {
		invited := "invited-" + suffix + "@mekoqa.test"
		const temp = "Temp0rary-pass!"
		if _, err := ctl.UpsertUser(t, cognitomock.UserSpec{Email: invited, Password: temp, Status: "FORCE_CHANGE_PASSWORD", FirstName: "Ina", LastName: "Vite"}); err != nil {
			return err
		}
		first, err := anon.Login(t, invited, temp)
		if err != nil {
			return err
		}
		if first.Challenge != "NEW_PASSWORD_REQUIRED" || first.Session == "" || first.AccessToken != "" {
			return fmt.Errorf("first login answered %+v, want the NEW_PASSWORD_REQUIRED challenge and no token", first)
		}
		weak, err := anon.Raw(t, http.MethodPost, "/auth/complete-new-password", mekoapi.NewPasswordRequest{
			Email: invited, NewPassword: "weak", ConfirmPassword: "weak", Session: first.Session})
		if err != nil {
			return err
		}
		if err := wantStatus("a weak new password", weak, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
			return err
		}
		done, err := anon.CompleteNewPassword(t, mekoapi.NewPasswordRequest{
			Email: invited, NewPassword: newPassword, ConfirmPassword: newPassword, Session: first.Session})
		if err != nil {
			return err
		}
		me, err := anon.WithToken(done.AccessToken).Me(t)
		if err != nil {
			return err
		}
		h.Set("account_id", me.AccountID)
		if me.Email != invited {
			return fmt.Errorf("/auth/me after the challenge says %q, want %q", me.Email, invited)
		}
		if _, err := anon.Login(t, invited, newPassword); err != nil {
			return fmt.Errorf("login with the new password: %w", err)
		}
		old, err := anon.Raw(t, http.MethodPost, "/auth/login", map[string]string{"email": invited, "password": temp})
		if err != nil {
			return err
		}
		return wantStatus("login with the temporary password after setting a new one", old, http.StatusUnauthorized, "")
	}); err != nil {
		return err
	}

	return t.Step("tokens_the_pool_did_not_issue_are_refused", func(h *core.StepHandle) error {
		good := session
		realToken := tokenOf(good)
		refused := func(what, token string) error {
			got, err := anon.WithToken(token).Raw(t, http.MethodGet, "/auth/me", nil)
			if err != nil {
				return err
			}
			return wantStatus(what, got, http.StatusUnauthorized, "UNAUTHORIZED")
		}
		mint := func(spec cognitomock.MintSpec) (string, error) {
			spec.Email = email
			return ctl.Mint(t, spec)
		}
		minusMinute := -60
		other, err := mint(cognitomock.MintSpec{Issuer: "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_NotOurPool"})
		if err != nil {
			return err
		}
		expired, err := mint(cognitomock.MintSpec{ExpiresIn: &minusMinute})
		if err != nil {
			return err
		}
		idToken, err := mint(cognitomock.MintSpec{TokenUse: "id"})
		if err != nil {
			return err
		}
		changed, err := stacktest.TamperClaims(realToken, func(c map[string]any) { c["sub"] = "someone-else" })
		if err != nil {
			return err
		}
		forged, err := stacktest.ForgeJWT(realToken)
		if err != nil {
			return err
		}
		for _, c := range []struct{ what, token string }{
			{"garbage as a bearer", "not-a-token"},
			{"a token from another pool", other},
			{"an expired token", expired},
			{"an ID token used as an access token", idToken},
			{"a payload changed under its signature", changed},
			{"a token signed by another key", forged},
		} {
			if err := refused(c.what, c.token); err != nil {
				return err
			}
		}
		none, err := anon.Raw(t, http.MethodGet, "/auth/me", nil)
		if err != nil {
			return err
		}
		if err := wantStatus("no bearer at all", none, http.StatusUnauthorized, "UNAUTHORIZED"); err != nil {
			return err
		}
		pat, err := good.CreateToken(t, mekoapi.CreateTokenRequest{Name: "mekoqa-login-" + suffix, TTLDays: 1})
		if err != nil {
			return err
		}
		defer func() {
			ctx, cancel := cleanupCtx()
			defer cancel()
			_ = good.RevokeToken(ctx, pat.Record.ID)
		}()
		if err := refused("a PAT on /auth/me, which takes JWTs only", pat.Value); err != nil {
			return err
		}
		still, err := good.Me(t)
		if err != nil || still.Email != email {
			return fmt.Errorf("the real token stopped working after the refusals: %+v %v", still, err)
		}
		out, err := good.Raw(t, http.MethodPost, "/auth/logout", nil)
		if err != nil {
			return err
		}
		if err := wantStatus("logout", out, http.StatusOK, ""); err != nil {
			return err
		}
		after, err := good.Raw(t, http.MethodGet, "/auth/me", nil)
		if err == nil {
			h.Set("me_after_logout", after.Status)
		}
		return nil
	})
}

func tokenOf(c *mekoapi.Client) string { return c.Token() }
