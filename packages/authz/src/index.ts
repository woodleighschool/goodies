export type Access = "none" | "view" | "edit";

export type Permissions<R extends string> = Readonly<Partial<Record<R, Access>>>;

export interface Requirement<R extends string> {
  resource: R;
  access: Access;
}

const levels: Record<Access, number> = { none: 0, view: 1, edit: 2 };

export function permissionLevel(value: string | undefined): Access {
  return value === "view" || value === "edit" ? value : "none";
}

export function can<R extends string>(
  permissions: Permissions<R> | undefined,
  resource: R,
  required: Access,
): boolean {
  const granted = permissions?.[resource] ?? "none";
  return (
    Object.hasOwn(levels, granted) &&
    Object.hasOwn(levels, required) &&
    levels[granted] >= levels[required]
  );
}

export function canAll<R extends string>(
  permissions: Permissions<R> | undefined,
  requirements: readonly Requirement<R>[],
): boolean {
  return requirements.every(({ resource, access }) => can(permissions, resource, access));
}

export class ForbiddenError extends Error {
  constructor() {
    super("forbidden");
    this.name = "ForbiddenError";
  }
}

export function requirePermission<R extends string>(
  permissions: Permissions<R> | undefined,
  resource: R,
  access: Access,
): void {
  requirePermissions(permissions, [{ resource, access }]);
}

export function requirePermissions<R extends string>(
  permissions: Permissions<R> | undefined,
  requirements: readonly Requirement<R>[],
): void {
  if (!canAll(permissions, requirements)) throw new ForbiddenError();
}

export interface NavigationItem<R extends string> {
  to?: string;
  permission?: Requirement<R>;
  disabled?: boolean;
  items?: readonly NavigationItem<R>[];
}

export function filterNavigation<R extends string, Item extends NavigationItem<R>>(
  items: readonly Item[],
  permissions: Permissions<R> | undefined,
): Item[] {
  return items.flatMap((item) => {
    if (item.disabled) return [];
    const children = item.items && filterNavigation(item.items, permissions);
    const allowed = item.permission
      ? can(permissions, item.permission.resource, item.permission.access)
      : !item.items;
    if (!allowed && !children?.length) return [];
    return [{ ...item, to: allowed ? item.to : children?.[0]?.to, items: children }];
  });
}

export function firstAccessibleTarget<R extends string>(
  items: readonly NavigationItem<R>[],
  permissions: Permissions<R> | undefined,
): string | undefined {
  for (const item of filterNavigation(items, permissions)) {
    if (item.to) return item.to;
    if (item.items) {
      const target = firstAccessibleTarget(item.items, permissions);
      if (target) return target;
    }
  }
  return undefined;
}
