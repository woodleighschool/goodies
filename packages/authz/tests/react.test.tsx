import assert from "node:assert/strict";
import { test } from "node:test";
import { renderToStaticMarkup } from "react-dom/server";

import { createAuthz } from "../src/react.js";

test("React bindings use only supplied permissions and deny without a provider", () => {
  const { AuthzProvider, Can, useCan } = createAuthz<"records">();
  function Edit() {
    return useCan({ resource: "records", access: "edit" }) ? "edit" : "read only";
  }
  assert.equal(
    renderToStaticMarkup(
      <Can resource="records" access="view">
        read
      </Can>,
    ),
    "",
  );
  assert.equal(
    renderToStaticMarkup(
      <AuthzProvider permissions={{ records: "view" }}>
        <Can resource="records" access="view">
          read
        </Can>
        <Edit />
      </AuthzProvider>,
    ),
    "readread only",
  );
  assert.equal(
    renderToStaticMarkup(
      <AuthzProvider permissions={{ records: "edit" }}>
        <Edit />
      </AuthzProvider>,
    ),
    "edit",
  );
});
