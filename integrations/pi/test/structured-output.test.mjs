import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import extension, {
  MAX_SCHEMA_BYTES,
  REPAIR_NUDGES,
  STRICT_MODEL,
  STRICT_PROVIDER,
  createStrictRepairController,
  enforceStrictResponsesRequest,
  validateStrictResponsesSSE,
} from "../extensions/structured-output.mjs";

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
        NO_MISTAKES_PI_STRUCTURED_OUTPUT: "1",
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

test("ordinary sessions remain silent when runtime dependencies are unavailable", async () => {
  const registrations = [];
  await withEnvironment(
    {
      NO_MISTAKES_GATE: undefined,
      NO_MISTAKES_PI_STRUCTURED_OUTPUT: undefined,
      NO_MISTAKES_JSON_SCHEMA_FILE: undefined,
      NO_MISTAKES_JSON_SCHEMA_SHA256: undefined,
    },
    () => extension(
      { registerTool: (tool) => registrations.push(tool) },
      { loadCompile: async () => { throw new Error("missing module"); } },
    ),
  );
  assert.deepEqual(registrations, []);
});

test("missing explicit opt-in registers no structured output surface", async () => {
  assert.deepEqual(await load({ type: "object" }, { NO_MISTAKES_PI_STRUCTURED_OUTPUT: undefined }), []);
});

test("valid gate reports missing runtime dependencies", async () => {
  await assert.rejects(
    load(
      { type: "object" },
      {},
      { loadCompile: async () => { throw new Error("missing module"); } },
    ),
    /npm ci --prefix integrations\/pi --ignore-scripts/,
  );
});

test("missing, malformed, oversized, and untrusted transports are refused", async () => {
  assert.deepEqual(await load(undefined), []);
  assert.deepEqual(await load("{"), []);
  assert.deepEqual(await load([]), []);
  assert.deepEqual(await load({ type: "array" }), []);
  assert.deepEqual(await load({ type: "object" }, { NO_MISTAKES_JSON_SCHEMA_SHA256: "0".repeat(64) }), []);
  assert.deepEqual(await load(`{"type":"object","description":"${"x".repeat(MAX_SCHEMA_BYTES)}"}`), []);

  const file = schemaFile({ type: "object" }, 0o644);
  try {
    const registrations = [];
    await withEnvironment(
      {
        NO_MISTAKES_GATE: "1",
        NO_MISTAKES_PI_STRUCTURED_OUTPUT: "1",
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

test("TypeBox refuses a semantically invalid schema", async () => {
  assert.deepEqual(await load({ type: "object", required: "summary" }), []);
});

test("transport variables are consumed before extension registration", async () => {
  const file = schemaFile({ type: "object" });
  const env = {
    NO_MISTAKES_GATE: "1",
    NO_MISTAKES_PI_STRUCTURED_OUTPUT: "1",
    NO_MISTAKES_JSON_SCHEMA_FILE: file.path,
    NO_MISTAKES_JSON_SCHEMA_SHA256: file.digest,
  };
  try {
    await extension({ registerTool() {} }, { env });
    assert.equal(env.NO_MISTAKES_PI_STRUCTURED_OUTPUT, undefined);
    assert.equal(env.NO_MISTAKES_JSON_SCHEMA_FILE, undefined);
    assert.equal(env.NO_MISTAKES_JSON_SCHEMA_SHA256, undefined);
  } finally {
    file.cleanup();
  }
});

test("review and test schemas are registered exactly without interpretation", async () => {
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

test("strict Responses activation marks the sole tool for required constrained sampling", async () => {
  const file = schemaFile({
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"],
    additionalProperties: false,
  });
  const tools = [];
  const handlers = new Map();
  const env = {
    NO_MISTAKES_GATE: "1",
    NO_MISTAKES_PI_STRUCTURED_OUTPUT: "1",
    NO_MISTAKES_PI_STRICT_RESPONSES: "1",
    NO_MISTAKES_JSON_SCHEMA_FILE: file.path,
    NO_MISTAKES_JSON_SCHEMA_SHA256: file.digest,
  };
  try {
    await extension({
      registerTool: (tool) => tools.push(tool),
      on: (name, handler) => handlers.set(name, handler),
      getActiveTools: () => ["structured_output"],
    }, { env, fetchTarget: { fetch: async () => { throw new Error("not called"); } } });
    assert.equal(tools.length, 1);
    assert.deepEqual(tools[0].constrainedSampling, { type: "json_schema", strict: "require" });
    assert.equal(typeof handlers.get("before_provider_request"), "function");
    assert.equal(env.NO_MISTAKES_PI_STRICT_RESPONSES, undefined);
  } finally {
    file.cleanup();
  }
});

test("strict Responses request hook is exact and refuses other routes", () => {
  const schema = {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"],
    additionalProperties: false,
  };
  const payload = {
    model: STRICT_MODEL,
    input: [{ role: "user", content: "review" }],
    stream: true,
    tools: [{ type: "function", name: "structured_output", parameters: schema, strict: true }],
    reasoning: { effort: "xhigh", summary: "auto" },
    include: ["reasoning.encrypted_content"],
    temperature: 1,
    top_p: 0.95,
    seed: 424242,
    max_output_tokens: 4096,
    store: false,
  };
  const pi = { getActiveTools: () => ["structured_output"] };
  const ctx = {
    model: { provider: STRICT_PROVIDER, id: STRICT_MODEL, api: "openai-responses" },
    thinkingLevel: "xhigh",
  };
  const result = enforceStrictResponsesRequest(payload, ctx, pi, schema);
  assert.equal(result.tool_choice, "required");
  assert.equal(result.parallel_tool_calls, false);
  assert.equal(result.include_reasoning, false);
  assert.equal(result.include, undefined);
  assert.equal(result.tools[0].strict, true);
  assert.deepEqual(result.tools[0].parameters, schema);
  assert.equal(result.reasoning.effort, "xhigh");
  assert.equal(result.model, STRICT_MODEL);
  assert.equal(result.temperature, 1);
  assert.equal(result.top_p, 0.95);
  assert.equal(result.seed, 424242);

  assert.throws(
    () => enforceStrictResponsesRequest(payload, { ...ctx, model: { ...ctx.model, api: "openai-completions" } }, pi, schema),
    /openai-responses/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest(payload, ctx, { getActiveTools: () => ["read", "structured_output"] }, schema),
    /only active tool/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, tools: [{ ...payload.tools[0], strict: false }] }, ctx, pi, schema),
    /strict structured_output/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, tools: [{ ...payload.tools[0], parameters: { type: "object" } }] }, ctx, pi, schema),
    /strict structured_output/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, temperature: 0.2 }, ctx, pi, schema),
    /pinned request parameters/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, top_p: 0.8 }, ctx, pi, schema),
    /pinned request parameters/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, seed: 99 }, ctx, pi, schema),
    /pinned request parameters/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, store: true }, ctx, pi, schema),
    /pinned request parameters/,
  );
  assert.throws(
    () => enforceStrictResponsesRequest({ ...payload, unexpected_override: true }, ctx, pi, schema),
    /unexpected request override/,
  );
});

test("Responses transport guard requires completed function-item status", () => {
  const event = (type, value) => `event: ${type}\ndata: ${JSON.stringify(value)}\n\n`;
  const valid = event("response.output_item.done", {
    type: "response.output_item.done",
    item: { type: "function_call", name: "structured_output", status: "completed" },
  }) + event("response.completed", {
    type: "response.completed",
    response: { status: "completed", error: null, incomplete_details: null, output: [
      { type: "function_call", name: "structured_output", status: "completed" },
    ] },
  }) + "data: [DONE]\n\n";
  assert.doesNotThrow(() => validateStrictResponsesSSE(valid));
  assert.throws(
    () => validateStrictResponsesSSE(valid.replace('"status":"completed"', '"status":"in_progress"')),
    /unsettled Responses function item/,
  );
  assert.throws(
    () => validateStrictResponsesSSE(event("response.incomplete", { type: "response.incomplete" })),
    /failed or incomplete/,
  );
});

test("same-session repair accepts native structured output after one fixed nudge", () => {
  const sent = [];
  const pi = { sendUserMessage: (text, options) => sent.push({ text, options }) };
  const validator = { Check: (value) => typeof value?.summary === "string" };
  const repair = createStrictRepairController(pi, validator);

  repair.beginProviderRequest();
  repair.observeMessage({ role: "assistant", content: [{ type: "text", text: "analysis prose" }], stopReason: "stop", rawStopReason: "completed" });
  repair.finishTurn({ message: { role: "assistant", stopReason: "stop", rawStopReason: "completed" } });
  assert.deepEqual(sent, [{
    text: REPAIR_NUDGES.missing,
    options: { deliverAs: "followUp", triggerTurn: true },
  }]);

  repair.beginProviderRequest();
  repair.observeMessage({ role: "assistant", content: [{
    type: "toolCall", name: "structured_output", arguments: { summary: "native authority" },
  }], stopReason: "toolUse", rawStopReason: "completed" });
  repair.accept({ summary: "native authority" });
  repair.finishTurn({ message: { role: "assistant", stopReason: "toolUse", rawStopReason: "completed" } });
  assert.deepEqual(repair.state(), { providerRequests: 2, repairNudges: 1, completed: true });
  assert.equal(sent.length, 1, "repair stays inside one controller/session and does not cold-retry");
});

test("repair classifies only objective format defects and caps provider turns", () => {
  const sent = [];
  const repair = createStrictRepairController(
    { sendUserMessage: (text) => sent.push(text) },
    { Check: (value) => typeof value?.summary === "string" },
  );

  repair.beginProviderRequest();
  repair.observeMessage({ role: "assistant", content: [{
    type: "toolCall", name: "structured_output", arguments: { summary: 17 },
  }], stopReason: "toolUse", rawStopReason: "completed" });
  repair.finishTurn({ message: { role: "assistant", stopReason: "toolUse", rawStopReason: "completed" } });
  assert.equal(sent[0], REPAIR_NUDGES.schema);

  repair.beginProviderRequest();
  assert.throws(
    () => repair.observeMessage({ role: "assistant", content: [{
      type: "toolCall", name: "structured-ouput", arguments: { summary: "ok" },
    }], stopReason: "toolUse", rawStopReason: "completed" }),
    /competing final tool call/,
  );
  assert.throws(
    () => repair.finishTurn({ message: { role: "assistant", stopReason: "toolUse" } }),
    /competing final tool call/,
  );

  assert.deepEqual(repair.state(), {
    providerRequests: 2,
    repairNudges: 1,
    completed: false,
  });
  assert.equal(sent.length, 1);

  const bounded = createStrictRepairController(
    { sendUserMessage: (text) => sent.push(text) },
    { Check: () => true },
  );
  bounded.beginProviderRequest();
  bounded.finishTurn({ message: { role: "assistant", stopReason: "stop", rawStopReason: "completed" } });
  bounded.beginProviderRequest();
  bounded.finishTurn({ message: { role: "assistant", stopReason: "stop", rawStopReason: "completed" } });
  bounded.beginProviderRequest();
  assert.throws(
    () => bounded.finishTurn({ message: { role: "assistant", stopReason: "stop", rawStopReason: "completed" } }),
    /exhausted two same-session format repairs/,
  );
  assert.deepEqual(bounded.state(), { providerRequests: 3, repairNudges: 2, completed: false });
  assert.throws(() => bounded.beginProviderRequest(), /provider-request cap/);
});

test("repair hard-stops multiple calls and does not nudge provider failures", () => {
  const sent = [];
  const validator = { Check: () => true };
  const repair = createStrictRepairController(
    { sendUserMessage: (text) => sent.push(text) },
    validator,
  );
  repair.beginProviderRequest();
  assert.throws(() => repair.observeMessage({ role: "assistant", content: [
    { type: "toolCall", name: "structured_output", arguments: {} },
    { type: "toolCall", name: "structured_output", arguments: {} },
  ], stopReason: "toolUse", rawStopReason: "completed" }), /multiple final tool calls/);
  assert.throws(
    () => repair.finishTurn({ message: { role: "assistant", stopReason: "toolUse", rawStopReason: "completed" } }),
    /multiple final tool calls/,
  );

  const duplicate = createStrictRepairController(
    { sendUserMessage: (text) => sent.push(text) },
    validator,
  );
  duplicate.beginProviderRequest();
  duplicate.observeMessage({ role: "assistant", content: [{
    type: "toolCall", name: "structured_output", arguments: {},
  }], stopReason: "toolUse", rawStopReason: "completed" });
  duplicate.accept({});
  duplicate.beginProviderRequest();
  duplicate.observeMessage({ role: "assistant", content: [{
    type: "toolCall", name: "structured_output", arguments: {},
  }], stopReason: "toolUse", rawStopReason: "completed" });
  assert.throws(() => duplicate.accept({}), /multiple completed structured_output calls/);

  const failed = createStrictRepairController(
    { sendUserMessage: (text) => sent.push(text) },
    validator,
  );
  failed.beginProviderRequest();
  failed.finishTurn({ message: { role: "assistant", stopReason: "error", errorMessage: "quota" } });
  assert.deepEqual(sent, []);
});

test("invalid Responses terminal statuses never trigger format repair", () => {
  for (const rawStopReason of [undefined, "queued", "in_progress", "incomplete.max_output_tokens"]) {
    const sent = [];
    const repair = createStrictRepairController(
      { sendUserMessage: (text) => sent.push(text) },
      { Check: () => true },
    );
    repair.beginProviderRequest();
    const message = { role: "assistant", stopReason: "stop" };
    if (rawStopReason !== undefined) message.rawStopReason = rawStopReason;
    assert.throws(
      () => repair.finishTurn({ message }),
      /invalid terminal transport boundary/,
      `raw terminal status ${rawStopReason ?? "absent"} must fail closed`,
    );
    assert.deepEqual(sent, []);
  }
});

test("route drift aborts the Pi turn and blocks result authority", async () => {
  const schema = {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"],
    additionalProperties: false,
  };
  const file = schemaFile(schema);
  const handlers = new Map();
  let aborts = 0;
  const env = {
    NO_MISTAKES_GATE: "1",
    NO_MISTAKES_PI_STRUCTURED_OUTPUT: "1",
    NO_MISTAKES_PI_STRICT_RESPONSES: "1",
    NO_MISTAKES_JSON_SCHEMA_FILE: file.path,
    NO_MISTAKES_JSON_SCHEMA_SHA256: file.digest,
  };
  const loadCompile = async () => () => ({ Check: () => true });
  try {
    await extension({
      registerTool() {},
      on: (name, handler) => handlers.set(name, handler),
      getActiveTools: () => ["structured_output"],
    }, { env, loadCompile, fetchTarget: { fetch: async () => { throw new Error("not called"); } } });
    const payload = {
      model: STRICT_MODEL,
      input: [{ role: "user", content: "review" }],
      stream: true,
      tools: [{ type: "function", name: "structured_output", parameters: schema, strict: true }],
      reasoning: { effort: "xhigh" },
      temperature: 0.2,
      top_p: 0.95,
      seed: 424242,
    };
    const returned = handlers.get("before_provider_request")(
      { payload },
      {
        model: { provider: STRICT_PROVIDER, id: STRICT_MODEL, api: "openai-responses" },
        thinkingLevel: "xhigh",
        abort: () => { aborts++; },
      },
    );
    assert.equal(returned, payload);
    assert.equal(aborts, 1);
    const blocked = handlers.get("tool_call")({ toolName: "structured_output" });
    assert.deepEqual(blocked, {
      block: true,
      terminate: true,
      reason: "strict no-mistakes gate payload lost pinned request parameters",
    });
  } finally {
    file.cleanup();
  }
});

test("malformed strict route control is refused", async () => {
  await assert.rejects(
    load({ type: "object" }, { NO_MISTAKES_PI_STRICT_RESPONSES: "true" }),
    /accepts only 1/,
  );
});

test("structured_output returns only exact JSON text and terminates", async () => {
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
