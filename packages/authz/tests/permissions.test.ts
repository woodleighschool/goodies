import assert from "node:assert/strict";
import { test } from "node:test";

import { can, canAll, type Access } from "../src/index.js";

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

test("all requirements must be satisfied", () => {
  const permissions = { records: "edit", reports: "view" } as const;
  const requirements = [
    { resource: "records", access: "edit" },
    { resource: "reports", access: "edit" },
  ] as const;
  assert.equal(canAll(permissions, requirements), false);
  assert.equal(canAll(undefined, []), true);
});
