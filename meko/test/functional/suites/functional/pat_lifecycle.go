package functional

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

const patTTLDays = 7

func init() {
	core.Register(core.Test{
		Name:        "functional/pat_lifecycle",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamAPIServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Mint a read-only PAT with a 7 day TTL and check its scopes, prefix and expiry. A second active token with the same name is refused with 409 and an unknown scope with 400. The list carries it, revoke marks it revoked and is idempotent, and the name is free again afterwards.",
		Run:         patLifecycle,
	})
}

func patLifecycle(t *core.T) error {
	api := apiFor(t)
	name := "mekoqa-pat-" + t.ResourceSuffix()
	var minted []string
	defer func() {
		ctx, cancel := cleanupCtx()
		defer cancel()
		for _, id := range minted {
			_ = api.RevokeToken(ctx, id)
		}
	}()

	var tok mekoapi.Token
	if err := t.Step("mint_read_only", func(h *core.StepHandle) error {
		var err error
		tok, err = api.CreateToken(t, mekoapi.CreateTokenRequest{Name: name, TTLDays: patTTLDays, Scopes: []string{mekoapi.ScopeRead}})
		if err != nil {
			return err
		}
		minted = append(minted, tok.Record.ID)
		h.Set("token_id", tok.Record.ID)
		rec := tok.Record
		if tok.Value == "" || rec.ID == "" {
			return fmt.Errorf("the response carries no token value or id: %+v", rec)
		}
		if rec.Name != name {
			return fmt.Errorf("record name %q, want %q", rec.Name, name)
		}
		if strings.Join(rec.Scopes, ",") != mekoapi.ScopeRead {
			return fmt.Errorf("record scopes %v, want [%s]", rec.Scopes, mekoapi.ScopeRead)
		}
		if rec.TokenPrefix == "" || !strings.HasPrefix(tok.Value, rec.TokenPrefix) {
			return fmt.Errorf("token_prefix %q is not a prefix of the minted token", rec.TokenPrefix)
		}
		want := time.Now().Add(patTTLDays * 24 * time.Hour)
		if d := rec.ExpiresAt.Sub(want); d < -24*time.Hour || d > 24*time.Hour {
			return fmt.Errorf("expires_at %s, want about %s", rec.ExpiresAt.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("duplicate_name_refused", func(*core.StepHandle) error {
		dup, err := api.CreateToken(t, mekoapi.CreateTokenRequest{Name: name, TTLDays: 1})
		if dup.Record.ID != "" {
			minted = append(minted, dup.Record.ID)
		}
		if st := mekoapi.StatusOf(err); st != http.StatusConflict {
			return fmt.Errorf("second active token named %q: %v, want a 409", name, err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("unknown_scope_refused", func(*core.StepHandle) error {
		bad, err := api.CreateToken(t, mekoapi.CreateTokenRequest{Name: name + "-bad", TTLDays: 1, Scopes: []string{"admin"}})
		if bad.Record.ID != "" {
			minted = append(minted, bad.Record.ID)
		}
		if st := mekoapi.StatusOf(err); st != http.StatusBadRequest {
			return fmt.Errorf("token with scope admin: %v, want a 400", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("listed_active", func(*core.StepHandle) error {
		rec, err := tokenRecord(t, api, tok.Record.ID)
		if err != nil {
			return err
		}
		if rec.TokenPrefix != tok.Record.TokenPrefix || rec.RevokedAt != nil {
			return fmt.Errorf("listed as %+v, want prefix %q and not revoked", rec, tok.Record.TokenPrefix)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("revoke", func(*core.StepHandle) error {
		if err := api.RevokeToken(t, tok.Record.ID); err != nil {
			return err
		}
		rec, err := tokenRecord(t, api, tok.Record.ID)
		if err != nil {
			return err
		}
		if rec.RevokedAt == nil {
			return fmt.Errorf("token %s is listed without revoked_at after revoke", tok.Record.ID)
		}
		if err := api.RevokeToken(t, tok.Record.ID); err != nil {
			return fmt.Errorf("second revoke: %w, want it to succeed", err)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("name_reusable_after_revoke", func(*core.StepHandle) error {
		again, err := api.CreateToken(t, mekoapi.CreateTokenRequest{Name: name, TTLDays: 1})
		if err != nil {
			return fmt.Errorf("mint %q after revoking the first: %w", name, err)
		}
		minted = append(minted, again.Record.ID)
		if again.Record.ID == tok.Record.ID {
			return fmt.Errorf("the new token reuses the revoked one's id %s", again.Record.ID)
		}
		return nil
	})
}

func tokenRecord(t *core.T, api *mekoapi.Client, id string) (mekoapi.TokenRecord, error) {
	list, err := api.ListTokens(t)
	if err != nil {
		return mekoapi.TokenRecord{}, err
	}
	for _, r := range list {
		if r.ID == id {
			return r, nil
		}
	}
	return mekoapi.TokenRecord{}, fmt.Errorf("token %s is not among the %d listed", id, len(list))
}
