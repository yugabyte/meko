# cognito-presignup-link

A Cognito **PreSignUp** Lambda trigger that links federated identities (Google,
GitHub, …) to an existing native account with the same **verified email**, so a
user is always one account (one `sub`) regardless of how they sign in.

## How it works

On a first-time federated sign-in Cognito invokes this trigger with
`triggerSource = PreSignUp_ExternalProvider`. The handler:

1. reads the incoming email,
2. searches the pool for an existing user with that email,
3. if found, calls `AdminLinkProviderForUser` to merge the provider in,
4. auto-confirms + auto-verifies so login proceeds seamlessly.

If no existing user is found, it does nothing and normal federated sign-up
continues (a fresh user is created).

## Deploy (AWS Console)

1. Lambda → Create function → Author from scratch
   - Name: `meko-dev-cognito-presignup-link`
   - Runtime: Node.js 22.x
   - Paste `index.mjs` (handler stays `index.handler`).
2. Configuration → Permissions → execution role → add inline policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["cognito-idp:ListUsers", "cognito-idp:AdminLinkProviderForUser"],
    "Resource": "arn:aws:cognito-idp:us-east-2:*:userpool/us-east-2_UJwIyxWqe"
  }]
}
```

3. Cognito → `meko-dev-login` → User pool properties → Lambda triggers →
   add **Pre sign-up** trigger → select this function.

## Notes

- The same function covers **all** providers; it derives the provider name from
  `event.userName` (`Google_…`, `GitHub_…`).
- The AWS SDK v3 client is bundled in the Node.js 18+/22 Lambda runtime, so no
  `npm install` is needed for console deploys.
