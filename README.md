# GOodies

`goodies` is a collection of go related, well, goodies...

## 🍬 Auth

`github.com/woodleighschool/goodies/auth` provides sessions, passwords, OIDC and API keys through `authn`, and resource permissions through `authz`.

The permission model is `none < view < edit`. Applications supply their resource catalogue and direct/inherited grants; `authz` merges them. Identity persistence, admission, role policy and HTTP endpoints stay in the application.

`@woodleighschool/authz` provides the same permission checks for TypeScript, pure navigation filtering, and React bindings under `@woodleighschool/authz/react`. It takes a permission map, not an API client or router.

## 🫧 Bloby

`github.com/woodleighschool/goodies/bloby` provides an opinionated blob lifecycle with file and S3 storage, direct and multipart ingestion, signed delivery, abandoned-upload cleanup and a concrete PostgreSQL registry. Applications own resource relationships, admission, product HTTP APIs and migration orchestration.

Objects move from pending to available. Direct and multipart describe how bytes are ingested, independently of that lifecycle.

`@woodleighschool/bloby-client` performs headless browser transfers after the server chooses an ingestion strategy. Applications retain request integration, finalization, attachment, progress UI and navigation.

## 🛠️ Development

Use `mise run deps`, then `mise run check`. `test-module` checks each Go module with `GOWORK=off` so workspace resolution cannot hide missing dependencies. `test-postgres` exercises Bloby's concrete registry against PostgreSQL.

## 📦 Releases

Release Please maintains one release PR with independent versions. Go module tags use paths such as `auth/vX.Y.Z` and `bloby/vX.Y.Z`; frontend tags use `authz/vX.Y.Z` and `bloby-client/vX.Y.Z`. Released paths under `packages/*` publish to npm from their release tags through trusted publishing. Each package owns its publish checks in `prepublishOnly` and build in `prepare`; adding a package to Release Please requires no workflow changes.
