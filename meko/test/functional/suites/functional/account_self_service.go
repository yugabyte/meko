package functional

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/account_self_service",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    10 * time.Minute,
		Description: "Everything a user changes about their own account. Forgot-password with the emailed code sets a new password, the old one stops working and a wrong code " +
			"or mismatched confirmation is refused; change-password needs the current password; the profile name, the onboarding flag and an email change confirmed with " +
			"its code show in /auth/me; /auth/me/usage counts the account's own datapacks; a PAT minted before the password change keeps working, and none of this is " +
			"reachable without a login.",
		Run: accountSelfService,
	})
}

func accountSelfService(t *core.T) error {
	accts, done := accountsFor(t, 1)
	defer done()
	a := accts[0]
	anon := anonymousAPI(t)
	pat := a.PAT()
	code := cognitomock.ConfirmationCode
	newPass := "Mekoqa-New-" + t.ResourceSuffix()[:8] + "!"
	newerPass := "Mekoqa-Newer-" + t.ResourceSuffix()[:8] + "!"
	login := func(email, pass string) (mekoapi.LoginResult, error) { return anon.Login(t, email, pass) }

	if err := t.Step("forgot_password_resets_it", func(*core.StepHandle) error {
		if err := restAs(t, anon, post, "/auth/forgot-password", map[string]string{"email": a.Email}, httpOK, nil); err != nil {
			return err
		}
		if err := refused(t, "confirm with a wrong code", anon, post, "/auth/forgot-password/confirm",
			map[string]string{"email": a.Email, "code": "000000", "new_password": newPass, "confirm_password": newPass}, 400, ""); err != nil {
			return err
		}
		if err := refused(t, "confirm with mismatched passwords", anon, post, "/auth/forgot-password/confirm",
			map[string]string{"email": a.Email, "code": code, "new_password": newPass, "confirm_password": newPass + "x"}, 400, ""); err != nil {
			return err
		}
		if err := restAs(t, anon, post, "/auth/forgot-password/confirm",
			map[string]string{"email": a.Email, "code": code, "new_password": newPass, "confirm_password": newPass}, httpOK, nil); err != nil {
			return err
		}
		if _, err := login(a.Email, a.Password); err == nil {
			return errors.New("the old password still logs in after the reset")
		}
		if _, err := login(a.Email, newPass); err != nil {
			return fmt.Errorf("the new password does not log in: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	var session *mekoapi.Client
	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("change_password_needs_the_current_one", func() error {
		res, err := login(a.Email, newPass)
		if err != nil {
			return err
		}
		session = a.API.WithToken(res.AccessToken)
		if err := refused(t, "change with a wrong current password", session, post, "/auth/change-password",
			map[string]string{"old_password": "wrong-" + newPass, "new_password": newerPass, "confirm_password": newerPass}, 400, ""); err != nil {
			if err2 := refused(t, "change with a wrong current password", session, post, "/auth/change-password",
				map[string]string{"old_password": "wrong-" + newPass, "new_password": newerPass, "confirm_password": newerPass}, http.StatusUnauthorized, ""); err2 != nil {
				return err
			}
		}
		if err := restAs(t, session, post, "/auth/change-password",
			map[string]string{"old_password": newPass, "new_password": newerPass, "confirm_password": newerPass}, httpOK, nil); err != nil {
			return err
		}
		if _, err := login(a.Email, newerPass); err != nil {
			return fmt.Errorf("the changed password does not log in: %w", err)
		}
		if _, err := a.WithPAT(pat.Value).ListDatapacks(t); err != nil {
			return fmt.Errorf("a PAT minted before the password change stopped working: %w", err)
		}
		return nil
	})
	if session == nil {
		return errors.Join(errs...)
	}
	step("profile_and_onboarding_show_in_me", func() error {
		if err := restAs(t, session, patch, "/auth/me/profile", map[string]string{"first_name": "Ada", "last_name": "Mekoqa"}, httpOK, nil); err != nil {
			return err
		}
		if err := restAs(t, session, patch, "/auth/me", map[string]bool{"has_completed_onboarding": true}, httpOK, nil); err != nil {
			return err
		}
		me, err := session.Me(t)
		if err != nil {
			return err
		}
		if me.FirstName != "Ada" || me.LastName != "Mekoqa" || !me.HasCompletedOnboarding {
			return fmt.Errorf("/auth/me answers %+v, want Ada Mekoqa with onboarding complete", me)
		}
		return nil
	})
	step("email_change_with_its_code", func() error {
		newEmail := "changed-" + t.ResourceSuffix() + "@mekoqa.test"
		if err := restAs(t, session, post, "/auth/me/email/start", map[string]string{"email": newEmail}, httpOK, nil); err != nil {
			return err
		}
		if err := refused(t, "verify with a wrong code", session, post, "/auth/me/email/verify", map[string]string{"code": "000000"}, 400, ""); err != nil {
			return err
		}
		if err := restAs(t, session, post, "/auth/me/email/verify", map[string]string{"code": code}, httpOK, nil); err != nil {
			return err
		}
		me, err := session.Me(t)
		if err != nil {
			return err
		}
		if me.Email != newEmail || !me.EmailVerified {
			return fmt.Errorf("/auth/me answers email %q verified %v, want %s verified", me.Email, me.EmailVerified, newEmail)
		}
		if _, err := login(newEmail, newerPass); err != nil {
			return fmt.Errorf("the new email does not log in: %w", err)
		}
		return nil
	})
	step("usage_counts_own_datapacks", func() error {
		var before struct {
			Count int `json:"datapack_count"`
		}
		if err := restAs(t, session, get, "/auth/me/usage", nil, httpOK, &before); err != nil {
			return err
		}
		dp, err := createDatapack(t, session, "usage-"+t.ResourceSuffix())
		if err != nil {
			return err
		}
		defer func() {
			ctx, cancel := cleanupCtx()
			defer cancel()
			_ = session.DeleteDatapack(ctx, dp.ID)
		}()
		var after struct {
			Count int `json:"datapack_count"`
		}
		if err := restAs(t, session, get, "/auth/me/usage", nil, httpOK, &after); err != nil {
			return err
		}
		if after.Count != before.Count+1 {
			return fmt.Errorf("usage counts %d datapacks after a create, %d before", after.Count, before.Count)
		}
		return nil
	})
	step("nothing_without_a_login", func() error {
		for _, r := range []struct{ method, path string }{
			{get, "/auth/me"}, {patch, "/auth/me/profile"}, {get, "/auth/me/usage"}, {post, "/auth/change-password"}, {post, "/auth/me/email/start"},
		} {
			if err := refused(t, "anonymous "+r.method+" "+r.path, anon, r.method, r.path, map[string]string{}, http.StatusUnauthorized, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(errs...)
}
