// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("locked frontend packages use the public npm registry", async () => {
  const lockfile = JSON.parse(
    await readFile(new URL("../package-lock.json", import.meta.url), "utf8"),
  );
  const packages = Object.entries(lockfile.packages).filter(([name]) => name !== "");
  assert.ok(packages.length > 0, "lockfile must contain frontend packages");
  for (const [name, entry] of packages) {
    assert.equal(typeof entry.resolved, "string", `${name} must have a resolved URL`);
    const url = new URL(entry.resolved);
    assert.equal(url.origin, "https://registry.npmjs.org", `${name} must use the public npm registry`);
    assert.equal(url.username, "", `${name} must not contain registry credentials`);
    assert.equal(url.password, "", `${name} must not contain registry credentials`);
  }
});
