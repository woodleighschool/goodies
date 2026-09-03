# GOodies

`goodies` is a collection of go related, well, goodies...

## 🍬 Auth

`github.com/woodleighschool/goodies/auth` provides credential and session primitives through `authn`, resource permissions through `authz`, and browser login through `browser`. The core packages have no Huma dependency.

The permission model is `none < view < edit`. Applications supply their resource catalogue and direct/inherited grants; `authz` merges them. Identity persistence, admission policy, roles and product endpoints stay in the application.

Construct `browser.New(authentication, browser.Config{Admit: authorization.HasAccess, SuccessRedirect: "/home", FailureRedirect: "/login", Logger: logger})` once per application. It applies admission before password or SSO login creates a session and again on authenticated requests. A valid bearer-token syntax takes precedence over the loaded SCS session. Mount `LimitPasswordLogin` before session loading and body validation on password login; SSO remains outside the password bucket.

`auth/huma`, imported as `authhuma`, owns Huma route registration, middleware and schemas. Call `RegisterSchemas` on the registry before registering operations. `OptionalAuth`, `RequireAuth`, permission guards and `RegisterSessions` take an explicit logger, log unexpected failures and return safe HTTP errors. Session registration retains `GET`, `POST` and `DELETE /api/session`; applications mount the browser service's `SSOStart` and `SSOCallback` handlers at their chosen routes. Raw HTTP guards live in `browser`.

`@woodleighschool/authz` provides the same permission checks for TypeScript, pure navigation filtering, and React bindings under `@woodleighschool/authz/react`. It takes a permission map, not an API client or router.

## 🫧 Bloby

`github.com/woodleighschool/goodies/bloby` owns blob ingestion, delivery, deletion and abandoned-upload cleanup. Construct one `Service` with `bloby.New(ctx, registry, config, logger)` and use `Begin` or `BeginDirect`, `Finalize`, `Write`, `Deliver` and `Delete`. Mount `service.TransferHandler()` for file transfers and run `service.RunCleanup(ctx)` in an application-owned goroutine. Applications own authorization, resource relationships and product HTTP endpoints.

Upload URLs write to separate staging keys. Finalization seals bytes before detecting their content type and hash, then publishes metadata once. Replayed uploads and concurrent retries cannot replace available content. Each attempt seals a unique candidate, then the registry atomically selects its key and metadata. Files pin an inode with a hard link; S3 copies a source pinned by ETag, including multipart copies above 5 GiB. This works with providers such as Garage that do not enforce conditional destination writes. Cleanup expires abandoned uploads, unused candidates and late staging writes even after their registry rows are gone. The file root or S3 bucket must be dedicated to Bloby; cleanup also expires incomplete server-side copies.

`bloby/pgxstore` implements the registry and exports versioned Goose migrations through `Migrations()`. Applications run those migrations to their pinned version using a separate `bloby_migrations` table before their own migrations, then keep resource foreign keys in their application schema.

`bloby/huma` adapts the shared upload action into a discriminated schema for HTTP APIs. `@woodleighschool/bloby-client` executes that transfer contract in the browser. Applications retain endpoint integration, finalization, attachment, progress UI and navigation.

## 🛠️ Development

Use `mise run deps`, then `mise run check`. `test-module` checks each Go module with `GOWORK=off` so workspace resolution cannot hide missing dependencies. `test-postgres` exercises Bloby's concrete registry against PostgreSQL.

## 📦 Releases

Release Please maintains one release PR with independent versions. Go module tags use paths such as `auth/vX.Y.Z` and `bloby/vX.Y.Z`; frontend tags use `authz/vX.Y.Z` and `bloby-client/vX.Y.Z`. Released paths under `packages/*` publish to npm from their release tags through trusted publishing. Each package owns its publish checks in `prepublishOnly` and build in `prepare`; adding a package to Release Please requires no workflow changes.
