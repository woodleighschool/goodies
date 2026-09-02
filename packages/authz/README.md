# Authz

Resource permissions: `none < view < edit`.

```ts
import { can, requirePermission } from "@woodleighschool/authz";

const permissions = { records: "view" } as const;
can(permissions, "records", "edit"); // false
requirePermission(permissions, "records", "view");
```

`canAll` and `requirePermissions` check every requirement. Failed assertions throw `ForbiddenError`; applications decide how to respond or redirect.

`filterNavigation` filters trees with optional `permission`, `to`, `disabled` and `items` fields, retaining application metadata. `firstAccessibleTarget` finds the first permitted destination.

## React

```tsx
import { createAuthz } from "@woodleighschool/authz/react";

const { AuthzProvider, useCan, Can } = createAuthz<"records">();

<AuthzProvider permissions={{ records: "view" }}>
  <Can resource="records" access="edit">
    Edit records
  </Can>
</AuthzProvider>;
```

The application supplies permissions and resource types. React is an optional peer dependency; the core entry point has no framework dependency.
