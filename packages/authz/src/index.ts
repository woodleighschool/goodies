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
