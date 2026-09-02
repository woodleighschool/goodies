import { createContext, useContext, type ReactNode } from "react";

import { can, type Permissions, type Requirement } from "./index.js";

export function createAuthz<R extends string>() {
  const Context = createContext<Permissions<R> | undefined>(undefined);

  function AuthzProvider({
    permissions,
    children,
  }: {
    permissions: Permissions<R> | undefined;
    children: ReactNode;
  }) {
    return <Context value={permissions}>{children}</Context>;
  }

  function useCan({ resource, access }: Requirement<R>): boolean {
    return can(useContext(Context), resource, access);
  }

  function Can({ resource, access, children }: Requirement<R> & { children: ReactNode }) {
    return useCan({ resource, access }) ? children : null;
  }

  return { AuthzProvider, useCan, Can };
}
