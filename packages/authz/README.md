# Authz

Resource permissions: `none < view < edit`.

```ts
import { can, canAll } from "@woodleighschool/authz";

const permissions = { records: "view" } as const;
can(permissions, "records", "edit"); // false
canAll(permissions, [{ resource: "records", access: "view" }]); // true
```

`canAll` checks every requirement. `permissionLevel` converts an untrusted string into a known access level. Applications own responses to denied access, navigation filtering, route guards and query integration.

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
