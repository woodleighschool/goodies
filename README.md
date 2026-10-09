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

`github.com/woodleighschool/goodies/bloby` owns ingestion, immutable publication, delivery, deletion and abandoned-upload cleanup. Construct one service with `bloby.New(ctx, registry, config, logger)`. An uploader declares its content before sending it: `bloby.Content` carries the size, SHA-256 and CRC64NVME that `bloby.Digest` computes. Use `Begin` or `BeginDirect` with the declaration for client uploads, `Write` for server-generated content, and `Finalize` to publish an upload once storage holds the declared bytes. Applications authorize operations and own attachment relationships, validation and product endpoints. A prefix is a storage namespace and type guard; it does not establish authorization or ownership.

The declaration is the object's identity, and storage enforces it. A single PUT is signed for the declared SHA-256 and length, so its URL can only ever store those bytes: an S3 provider rejects any other body, and so does the file transfer handler. A multipart upload is created with a whole-object CRC64NVME, and each part URL is signed for that part's CRC64NVME. `Finalize` assembles a multipart upload from the parts the provider holds, compares the provider's own checksum of the object with the declaration, detects the content type from the first 3072 bytes and publishes. The server never reads an object to publish it, and an object whose provider reports no whole-object checksum is not published. A multipart object's SHA-256 is therefore the uploader's word; storage vouches for its size and CRC64NVME. S3 providers must support checksum headers on presigned requests and full-object CRC64NVME multipart uploads, as AWS, R2 and Garage do. Browser uploads send the signed `x-amz-checksum-*` header, which the bucket's CORS policy must allow.

An object keeps one key for life, `_objects/<id>/<filename>`. A replayed upload URL can only resend the bytes already there, and concurrent finalizers publish once. `Finalize` is idempotent and returns an error matching `ErrContentMismatch` when storage holds bytes other than the declared ones, or `ErrObjectNotFound` when the upload has not arrived; both also match `ErrInvalidInput`.

`Open` returns a stream. `Deliver` uses standard file HTTP range serving or a signed S3 redirect. Mount `TransferHandler` for file transfers and run `RunCleanup(ctx)` in an application-owned goroutine. Cleanup expires pending uploads, removes stored bytes that no object owns, such as an upload that lands after its object was deleted, and aborts multipart uploads that no pending object owns. Under `Config.ReferencedPrefixes` it also removes published objects that no foreign key references after the same age, such as an upload its owner never attached; leave out prefixes that hold a browsable library. The file root or S3 bucket must be dedicated to Bloby.

`bloby/pgxstore` owns the PostgreSQL registry and its schema. Call `pgxstore.Migrate(ctx, pool)` before application migrations that reference storage objects. It applies the schema required by the dependency, using its own migration table and lock. Applications own migration timing and foreign keys. A pending object records its declaration, so only `Object.Available` says whether it is published. Reference constraints prevent deletion of attached content; application retention policy decides when available objects can be discarded. `DeleteUnreferenced` is best effort after a committed application mutation.

`bloby/huma` describes the upload action as a discriminated HTTP schema. `@woodleighschool/bloby-client` digests a blob for its declaration and executes direct or multipart transfers, including part checksums, part signing and retries. Applications provide endpoint callbacks and retain publication, attachment, cancellation UI, toasts and navigation.

## 🔒 PostgreSQL locks

`github.com/woodleighschool/goodies/pglock` owns the dedicated connection needed for a PostgreSQL advisory lock. `Locker.Try` skips work when another replica holds the key; `Locker.With` waits. Closing the connection releases the session lock, including after callback failure or panic. Work retains the full application pool. Applications choose lock keys and the work to serialize.

## 🧪 Testing boundaries

Credential, session, OIDC and admission behavior is tested in `authn`; raw HTTP and Huma packages test their own adapters. Permission evaluation is tested without a database. Applications test their identity queries, password rules, roles and endpoint composition.

Bloby tests admission of declared bytes, replay, publication and cleanup on the file backend and on an in-process S3 fixture that applies the checksum checks real providers apply. PostgreSQL tests exercise actual constraints, concurrent state transitions, migration and reference-safe deletion. `mise //bloby:test-s3` runs the provider interoperability test against a real S3 endpoint named by `BLOBY_TEST_S3_ENDPOINT`, `BLOBY_TEST_S3_REGION`, `BLOBY_TEST_S3_BUCKET`, `BLOBY_TEST_S3_ACCESS_KEY` and `BLOBY_TEST_S3_SECRET_KEY`; `BLOBY_TEST_S3_PATH_STYLE=false` selects virtual-hosted addressing. It removes what it stores and never runs cleanup against the bucket. The browser client tests transfers and cancellation; applications test attachment and navigation behavior. Generic library behavior is not repeated through each application's full HTTP stack.

## 🛠️ Development

Use `mise install`, then `mise run deps` and `mise run check`. Mise installs the shared Git hooks for formatting, workflow checks, and conventional commit validation. The root Mise config owns tool versions and aggregate tasks; each Go module and Node package owns its tasks in `.mise.toml`. Run a component directly with commands such as `mise //auth:test`, `mise //bloby:tidy-check`, or `mise //packages/authz:build`.

Go module tasks use `GOWORK=off` so workspace resolution cannot hide missing dependencies. Node tasks share one workspace install and call the package's scripts. `mise run test-postgres` exercises Bloby's registry and advisory locks against PostgreSQL; run either component's `test-postgres` task to target it individually.

## 📦 Releases

Release Please maintains one release PR with independent versions. Go module tags use paths such as `auth/vX.Y.Z` and `bloby/vX.Y.Z`; frontend tags use `authz/vX.Y.Z` and `bloby-client/vX.Y.Z`. Released paths under `packages/*` publish to npm from their release tags through trusted publishing. Each package owns its publish checks in `prepublishOnly` and build in `prepare`; adding a package to Release Please requires no workflow changes.

All configured conventional commit types, including `chore` and `ci`, can trigger releases for the packages they touch. Root-only tooling and workflow changes do not release packages. Before 1.0, features bump the patch version and breaking changes bump the minor version.
