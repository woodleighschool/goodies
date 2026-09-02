# GOodies

`goodies` is a collection of go related, well, goodies...

## 🍬 Auth

`github.com/woodleighschool/goodies/auth` provides sessions, passwords, OIDC and API keys through `authn`, and resource permissions through `authz`.

The permission model is `none < view < edit`. Applications supply their resource catalogue and direct/inherited grants; `authz` merges them. Identity persistence, admission, role policy and HTTP endpoints stay in the application.

`@woodleighschool/authz` provides the same permission checks for TypeScript, pure navigation filtering, and React bindings under `@woodleighschool/authz/react`. It takes a permission map, not an API client or router.

## 🛠️ Development

Use `mise run deps`, then `mise run check`. `test-module` checks `auth/` with `GOWORK=off` so workspace resolution cannot hide missing dependencies.

## 📦 Releases

Release Please maintains one release PR with independent versions. Go module tags use `auth/vX.Y.Z`; frontend tags use `authz/vX.Y.Z`. A frontend release publishes `@woodleighschool/authz` to npm through trusted publishing.
