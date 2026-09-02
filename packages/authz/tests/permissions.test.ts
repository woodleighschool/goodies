import assert from "node:assert/strict";
import { test } from "node:test";

import {
  can,
  canAll,
  filterNavigation,
  firstAccessibleTarget,
  ForbiddenError,
  requirePermission,
  requirePermissions,
  type Access,
  type NavigationItem,
} from "../src/index.js";

test("none < view < edit, absent grants deny access", () => {
  const access: Access[] = ["none", "view", "edit"];
  access.forEach((granted, i) =>
    access.forEach((required, j) => {
      assert.equal(can({ records: granted }, "records", required), i >= j);
    }),
  );
  assert.equal(can(undefined, "records", "view"), false);
  assert.equal(can({}, "records", "edit"), false);
  assert.equal(can({ records: "owner" as Access }, "records", "view"), false);
  assert.equal(can({ records: "edit" }, "records", "owner" as Access), false);
});

test("assertions enforce all requirements without routing", () => {
  const permissions = { records: "edit", reports: "view" } as const;
  const requirements = [
    { resource: "records", access: "edit" },
    { resource: "reports", access: "edit" },
  ] as const;
  assert.equal(canAll(permissions, requirements), false);
  assert.throws(() => requirePermissions(permissions, requirements), ForbiddenError);
  assert.doesNotThrow(() => requirePermission(permissions, "records", "edit"));
  assert.equal(canAll(undefined, []), true);
});

test("navigation retains metadata, prunes empty branches, and chooses an allowed target", () => {
  interface Item extends NavigationItem<"records"> {
    label: string;
    items?: readonly Item[];
  }
  const items = [
    {
      label: "Records",
      to: "/records",
      items: [
        { label: "Edit", to: "/records/edit", permission: { resource: "records", access: "edit" } },
        { label: "Read", to: "/records/read", permission: { resource: "records", access: "view" } },
      ],
    },
    { label: "Hidden", to: "/disabled", disabled: true },
  ] satisfies Item[];
  const original = JSON.stringify(items);
  const filtered = filterNavigation(items, { records: "view" });
  assert.equal(filtered[0]?.label, "Records");
  assert.equal(filtered[0]?.to, "/records/read");
  assert.equal(filtered[0]?.items?.length, 1);
  assert.equal(firstAccessibleTarget(items, { records: "view" }), "/records/read");
  assert.deepEqual(filterNavigation(items, undefined), []);
  assert.equal(JSON.stringify(items), original);
});
