package functional

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/free_tier_limits",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Description: "The free tier's caps as a free user meets them. Datapack creations count towards the cap even after a delete and /auth/me/usage counts them; " +
			"the create past the cap is refused with a tier-limit answer; a knowledge-base file over the free per-file size is refused; once the account moves to a paid " +
			"tier /auth/me shows it and the next create succeeds, so the tier is read live and not cached.",
		Run: freeTierLimits,
	})
}

func freeTierLimits(t *core.T) error {
	o := t.Options
	maxDatapacks := o.GetInt("functional.free_tier.datapacks", 10)
	maxFile := o.GetInt("functional.free_tier.kb_file_bytes", 5<<20)
	accts, done := accountsFor(t, 1)
	defer done()
	a := accts[0]
	stack := clusterEnv(t).Stack()
	if _, err := stack.YSQL(t, fmt.Sprintf("UPDATE meko_system.users SET user_type = 'free' WHERE user_email = '%s' RETURNING 1", a.Email)); err != nil {
		return fmt.Errorf("put %s on the free tier: %w", a.Email, err)
	}
	var made []string
	defer func() {
		ctx, cancel := cleanupCtx()
		defer cancel()
		for _, id := range made {
			_ = a.API.DeleteDatapack(ctx, id)
		}
	}()

	type usageCounts struct {
		Creations int `json:"datapack_creations"`
		Count     int `json:"datapack_count"`
	}
	usage := func() (usageCounts, error) {
		var u usageCounts
		err := restAs(t, a.API, get, "/auth/me/usage", nil, httpOK, &u)
		return u, err
	}
	if err := t.Step("creations_count_towards_the_cap", func(h *core.StepHandle) error {
		start, err := usage()
		if err != nil {
			return err
		}
		h.Set("usage_at_start", fmt.Sprintf("%+v", start))
		if start.Creations >= maxDatapacks-1 {
			return fmt.Errorf("a new account starts with %d datapack creations of the free %d, no room to test the cap", start.Creations, maxDatapacks)
		}
		for i := start.Creations; i < maxDatapacks; i++ {
			dp, err := createDatapack(t, a.API, fmt.Sprintf("free-%d-%s", i, t.ResourceSuffix()))
			if err != nil {
				return fmt.Errorf("create %d of the free %d: %w", i+1, maxDatapacks, err)
			}
			made = append(made, dp.ID)
			if i == start.Creations {
				if err := a.API.DeleteDatapack(t, dp.ID); err != nil {
					return err
				}
				made = made[1:]
			}
		}
		end, err := usage()
		if err != nil {
			return err
		}
		h.Set("usage", fmt.Sprintf("%+v", end))
		if end.Creations != maxDatapacks || end.Count != start.Count+len(made) {
			return fmt.Errorf("usage counts %d creations and %d live, want %d creations and %d live (one of ours deleted)", end.Creations, end.Count, maxDatapacks, start.Count+len(made))
		}
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("the_create_past_the_cap_is_refused", func() error {
		r, err := a.API.Raw(t, post, "/datapacks", mekoapi.CreateDatapackRequest{Name: "free-over-" + t.ResourceSuffix()})
		if err != nil {
			return err
		}
		if r.Status/100 == 2 {
			return fmt.Errorf("a free account created datapack %d past its cap of %d: %s", maxDatapacks+1, maxDatapacks, truncateBody(r.Body))
		}
		if r.Status != http.StatusForbidden && r.Status != http.StatusPaymentRequired && r.Status != http.StatusTooManyRequests {
			return fmt.Errorf("the create past the cap answered %d, want a tier-limit refusal: %s", r.Status, truncateBody(r.Body))
		}
		if !strings.Contains(strings.ToLower(string(r.Body)), "limit") {
			return fmt.Errorf("the refusal does not say a limit was reached: %s", truncateBody(r.Body))
		}
		return nil
	})
	step("an_oversized_file_is_refused", func() error {
		if len(made) == 0 {
			return errors.New("no datapack left to upload into")
		}
		big := []byte(strings.Repeat("free tier file line\n", maxFile/20+100))
		_, err := a.API.UploadKBFile(t, made[0], "big-"+t.ResourceSuffix()+".txt", big)
		if err == nil {
			return fmt.Errorf("a %d-byte file was accepted on the free tier, over its %d-byte cap", len(big), maxFile)
		}
		if st := mekoapi.StatusOf(err); st != http.StatusRequestEntityTooLarge && st != http.StatusForbidden && st != http.StatusBadRequest {
			return fmt.Errorf("the oversized upload answered %d, want a size refusal: %v", st, err)
		}
		return nil
	})
	step("a_paid_tier_lifts_the_cap", func() error {
		if _, err := stack.YSQL(t, fmt.Sprintf("UPDATE meko_system.users SET user_type = 'pro' WHERE user_email = '%s' RETURNING 1", a.Email)); err != nil {
			return fmt.Errorf("move %s to pro: %w", a.Email, err)
		}
		me, err := a.API.Me(t)
		if err != nil {
			return err
		}
		if me.UserType != "pro" {
			return fmt.Errorf("/auth/me says tier %q after the move to pro", me.UserType)
		}
		dp, err := createDatapack(t, a.API, "free-lifted-"+t.ResourceSuffix())
		if err != nil {
			return fmt.Errorf("the create after the move to pro is still refused: %w", err)
		}
		made = append(made, dp.ID)
		return nil
	})
	return errors.Join(errs...)
}
