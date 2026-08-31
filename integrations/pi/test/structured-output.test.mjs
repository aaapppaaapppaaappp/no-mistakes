import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import extension, { MAX_SCHEMA_BYTES } from "../extensions/structured-output.mjs";

const posixTest = process.platform === "win32" ? test.skip : test;

async function withEnvironment(values, fn) {
  const previous = {};
  for (const [key, value] of Object.entries(values)) {
    previous[key] = process.env[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  try {
    return await fn();
  } finally {
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
}

function schemaFile(schema, mode = 0o600) {
  const dir = mkdtempSync(join(tmpdir(), "no-mistakes-pi-extension-test-"));
  const path = join(dir, "schema.json");
  const text = typeof schema === "string" ? schema : JSON.stringify(schema);
  writeFileSync(path, text, { mode });
  chmodSync(path, mode);
  return {
    path,
    digest: createHash("sha256").update(text).digest("hex"),
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

async function load(schema, env = {}, options = {}) {
  const registrations = [];
  const file = schema === undefined ? undefined : schemaFile(schema);
  try {
    await withEnvironment(
      {
        NO_MISTAKES_GATE: "1",
        NO_MISTAKES_JSON_SCHEMA_FILE: file?.path,
        NO_MISTAKES_JSON_SCHEMA_SHA256: file?.digest,
        ...env,
      },
      () => extension({ registerTool: (tool) => registrations.push(tool) }, options),
    );
    return registrations;
  } finally {
    file?.cleanup();
  }
}

test("ordinary sessions register no structured output surface", async () => {
  const file = schemaFile({ type: "object" });
  try {
    const registrations = [];
    await withEnvironment(
      {
        NO_MISTAKES_GATE: undefined,
        NO_MISTAKES_JSON_SCHEMA_FILE: file.path,
        NO_MISTAKES_JSON_SCHEMA_SHA256: file.digest,
      },
      () => extension({ registerTool: (tool) => registrations.push(tool) }),
    );
    assert.deepEqual(registrations, []);
  } finally {
    file.cleanup();
  }
});

test("missing, malformed, oversized, and untrusted transports are refused", async () => {
  assert.deepEqual(await load(undefined), []);
  assert.deepEqual(await load("{"), []);
  assert.deepEqual(await load([]), []);
  assert.deepEqual(await load({ type: "array" }), []);
  assert.deepEqual(await load({ type: "object" }, { NO_MISTAKES_JSON_SCHEMA_SHA256: "0".repeat(64) }), []);
  assert.deepEqual(await load(`{"type":"object","description":"${"x".repeat(MAX_SCHEMA_BYTES)}"}`), []);

  if (process.platform !== "win32") {
    const file = schemaFile({ type: "object" }, 0o644);
    try {
      const registrations = [];
      await withEnvironment(
        {
          NO_MISTAKES_GATE: "1",
          NO_MISTAKES_JSON_SCHEMA_FILE: file.path,
          NO_MISTAKES_JSON_SCHEMA_SHA256: file.digest,
        },
        () => extension({ registerTool: (tool) => registrations.push(tool) }),
      );
      assert.deepEqual(registrations, []);
    } finally {
      file.cleanup();
    }
  }
});

posixTest("TypeBox refuses a semantically invalid schema", async () => {
  assert.deepEqual(await load({ type: "object", required: "summary" }), []);
});

test("Windows exposes no structured output surface", async () => {
  assert.deepEqual(await load({ type: "object" }, {}, { platform: "win32" }), []);
});

posixTest("review and test schemas are registered exactly without interpretation", async () => {
  const review = {
    type: "object",
    properties: {
      findings: { type: "array", items: { type: "object", required: ["review_scope"] } },
      risk_scope: { type: "string", enum: ["source-or-external", "pipeline-owned-delivery"] },
    },
    required: ["findings", "risk_scope"],
  };
  const testSchema = {
    type: "object",
    properties: {
      findings: { type: "array" },
      artifacts: {
        type: "array",
        items: { type: "object", properties: { label: { type: "string" } }, required: ["label"] },
      },
    },
    required: ["findings", "artifacts"],
  };

  assert.deepEqual((await load(review))[0].parameters, review);
  assert.deepEqual((await load(testSchema))[0].parameters, testSchema);
});

posixTest("structured_output returns only exact JSON text and terminates", async () => {
  const [tool] = await load({
    type: "object",
    properties: { artifacts: { type: "array" }, summary: { type: "string" } },
    required: ["artifacts", "summary"],
  });
  assert.equal(tool.name, "structured_output");
  const params = { artifacts: [{ label: "proof", path: "/tmp/proof.log" }], summary: "passed" };
  const result = await tool.execute("call-1", params);
  assert.deepEqual(result.content, [{ type: "text", text: JSON.stringify(params) }]);
  assert.equal(result.terminate, true);
});
