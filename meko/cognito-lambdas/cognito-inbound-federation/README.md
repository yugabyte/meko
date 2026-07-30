# cognito-inbound-federation

A Cognito **Inbound Federation** Lambda trigger that fills placeholder values for
required user-pool attributes the external IdP didn't supply, so federated
sign-in can't fail with `attributes required: [family_name]`.

## Why this exists

Our user pool marks `given_name` / `family_name` as **required**, and you can't
un-require an attribute after a pool is created. Google (and other IdPs) only
return those claims when the account actually has them, so a Google account with
no last name fails federation with:

```
error=invalid_request&error_description=attributes+required:+[family_name]
```

This error is thrown *inside* Cognito, before any auth code reaches `api_server`,
so it can't be patched from our Go backend. The inbound-federation trigger runs
before Cognito validates required attributes, on **every** federated sign-in
(new and existing users), which is the one place we can inject a value.

## How it works

On a federated sign-in Cognito invokes this trigger with
`triggerSource = InboundFederation_ExternalProvider`. The handler first calls
`AdminGetUser` to tell **new** users from **returning** ones:

- **Returning user** → no-op: it returns without setting `userAttributesToMap`,
  so Cognito keeps the stored attributes and updates only the claims the IdP
  actually sent. (Google never sends `family_name`, so a value the user
  corrected during onboarding is preserved.)
- **New user** → it:
  1. merges the IdP attributes (`userInfo`, falling back to `idToken`),
  2. sets `given_name` / `family_name` to `"-"` when missing,
  3. derives `name` from them when missing,
  4. returns them in `response.userAttributesToMap`.

The user is created with placeholders; the onboarding flow (`ConfirmNameModal`)
then collects the real first/last name, which `api_server`'s `UpdateProfile`
writes back to both the local DB **and** Cognito (via
`AdminUpdateUserAttributes`). The idempotent no-op above is what keeps that
corrected name from being re-stamped with `"-"` on the next sign-in.

## Idempotency (why this trigger fires every sign-in)

Inbound federation runs on **every** federated sign-in, not just the first. A
naive handler that always stamps `"-"` for a missing `family_name` would
overwrite the user's real name in Cognito on each login. The `AdminGetUser`
check restricts placeholder injection to brand-new users; everyone else is a
no-op, so Cognito and the local DB stay consistent.

## Relationship to `cognito-presignup-account-link`

These are **different triggers** and both can be attached at once:

| Trigger | Job | Runs |
|---|---|---|
| `cognito-presignup-account-link` (Pre sign-up) | link a federated identity → existing email account | first federation only |
| `cognito-inbound-federation` (Inbound federation) | fill missing required attrs | every federated sign-in |

A PreSignUp trigger can only set `autoConfirmUser` / `autoVerifyEmail` /
`autoVerifyPhone` — it has no `userAttributesToMap`, so it **cannot** fill
`family_name`. That's why this separate trigger is required.

## Deploy (AWS Console)

1. Lambda → Create function → Author from scratch
   - Name: `meko-dev-cognito-inbound-federation`
   - Runtime: Node.js 22.x
   - Paste `index.mjs` (handler stays `index.handler`).
   - **IAM:** add `cognito-idp:AdminGetUser` on the user pool to the function's
     execution role. The handler uses it to tell new vs. returning users apart
     (idempotency); without it every sign-in throws and federation fails.
2. Cognito → user pool → User pool properties → Lambda triggers →
   add **Inbound federation** trigger → select this function.
3. **Map the attributes you inject.** In the Google (and any other) IdP
   attribute mapping, ensure `given_name`, `family_name`, and `name` are mapped
   to the matching Cognito attributes. Values set in `userAttributesToMap` are
   filtered by this mapping — unmapped attributes are dropped and the
   required-attribute check fails again.

## Notes

- The same function covers **all** providers.
- The AWS SDK v3 client (`@aws-sdk/client-cognito-identity-provider`) is bundled
  in the Node.js 18+/22 Lambda runtime, so no `npm install` is needed for console
  deploys — but the execution role still needs the `AdminGetUser` permission
  above.
