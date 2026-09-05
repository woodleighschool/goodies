# GOodies

`goodies` is a collection of go related, well, goodies...

## 🍬 Auth

`github.com/woodleighschool/goodies/auth/authn` owns password hashing and verification, API-key credentials, SCS sessions, password-login throttling and OIDC. Construct one service with `authn.New(ctx, identities, sessions, config)`. OIDC discovery finishes during construction; the service has no mutable configuration phase.

Applications supply eligible identities through `authn.Store` and enforce their own password policy before calling `HashPassword`. `Config.Admit` optionally checks additional application access on login and authenticated requests. Woodgate uses resource permissions for this check; Woodstar filters eligibility in its identity queries. A valid bearer-token syntax takes precedence over the loaded SCS session.

Mount `LimitPasswordLogin` before session loading and body validation on password login. Mount `SSOStart` and `SSOCallback` at application-chosen routes. Applications own SCS persistence, cookies, cross-origin protection, destinations, account responses and resource endpoints.

`auth/http`, imported as `authhttp`, provides raw HTTP guards. `auth/huma`, imported as `authhuma`, provides Huma guards, operation metadata and session endpoints. `RegisterSessions` takes separate session, password and logout API surfaces. Logout loads and destroys the session independently of identity lookup or current admission, so losing application access cannot prevent sign-out. Core packages do not import Huma.

`auth/authz` evaluates application-supplied grants against an application-owned resource catalogue using `none < view < edit`. Roles, group membership, identity persistence and resource definitions belong to applications. Call `authhuma.RegisterSchemas` before registering Huma operations that use authorization enums.

`@woodleighschool/authz` supplies TypeScript permission checks and optional React bindings under `@woodleighschool/authz/react`. Applications supply permission maps and own navigation, route guards and query integration.

## 🫧 Bloby

`github.com/woodleighschool/goodies/bloby` owns ingestion, immutable publication, delivery, deletion and abandoned-upload cleanup. Construct one service with `bloby.New(ctx, registry, config, logger)`. Use `Begin` or `BeginDirect` for client uploads, `Write` for server-generated content, and `Finalize` to inspect and publish uploaded bytes. Applications authorize operations and own attachment relationships, validation and product endpoints. A prefix is a storage namespace and type guard; it does not establish authorization or ownership.

Signed uploads write staging keys. Finalization seals a unique candidate before detecting content type and hashing, then the registry atomically selects one immutable representation. Replayable uploads and concurrent finalizers cannot replace published bytes. Files pin the staging inode with a hard link; S3 copies a source pinned by ETag, including multipart copies above 5 GiB.

`Open` returns a stream. `Deliver` uses standard file HTTP range serving or a signed S3 redirect. Mount `TransferHandler` for file transfers and run `RunCleanup(ctx)` in an application-owned goroutine. Cleanup expires pending uploads, late staging writes and unselected candidates. The file root or S3 bucket must be dedicated to Bloby.

`bloby/pgxstore` owns the PostgreSQL registry and its schema. Call `pgxstore.Migrate(ctx, pool)` before application migrations that reference storage objects. It applies the schema required by the dependency, using its own migration table and lock. Applications own migration timing and foreign keys. Reference constraints prevent deletion of attached content; application retention policy decides when available objects can be discarded. `DeleteUnreferenced` is best effort after a committed application mutation.

`bloby/huma` describes the upload action as a discriminated HTTP schema. `@woodleighschool/bloby-client` executes direct or multipart transfers, including part signing, retries and multipart completion. Applications provide endpoint callbacks and retain publication, attachment, cancellation UI, toasts and navigation.

## 🔒 PostgreSQL locks

`github.com/woodleighschool/goodies/pglock` owns the dedicated connection needed for a PostgreSQL advisory lock. `Locker.Try` skips work when another replica holds the key; `Locker.With` waits. Closing the connection releases the session lock, including after callback failure or panic. Work retains the full application pool. Applications choose lock keys and the work to serialize.

## 🧪 Testing boundaries

Credential, session, OIDC and admission behavior is tested in `authn`; raw HTTP and Huma packages test their own adapters. Permission evaluation is tested without a database. Applications test their identity queries, password rules, roles and endpoint composition.

Bloby tests storage protocols, replay, publication and cleanup. PostgreSQL tests exercise actual constraints, concurrent state transitions, migration and reference-safe deletion. The browser client tests transfers and cancellation; applications test attachment and navigation behavior. Generic library behavior is not repeated through each application's full HTTP stack.

## 🛠️ Development

Use `mise run deps`, then `mise run check`. `test-module` checks each Go module with `GOWORK=off` so workspace resolution cannot hide missing dependencies. `test-postgres` exercises Bloby's registry and advisory locks against PostgreSQL.

## 📦 Releases

Release Please maintains one release PR with independent versions. Go module tags use paths such as `auth/vX.Y.Z` and `bloby/vX.Y.Z`; frontend tags use `authz/vX.Y.Z` and `bloby-client/vX.Y.Z`. Released paths under `packages/*` publish to npm from their release tags through trusted publishing. Each package owns its publish checks in `prepublishOnly` and build in `prepare`; adding a package to Release Please requires no workflow changes.
