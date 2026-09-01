import { createHash, timingSafeEqual } from "node:crypto";
import { closeSync, constants, fstatSync, openSync, readSync } from "node:fs";
import { isAbsolute } from "node:path";

import draft07MetaSchema from "./draft-07-meta-schema.mjs";

const GATE_MARKER = "NO_MISTAKES_GATE";
const SCHEMA_PATH = "NO_MISTAKES_JSON_SCHEMA_FILE";
const SCHEMA_DIGEST = "NO_MISTAKES_JSON_SCHEMA_SHA256";
const STRUCTURED_OUTPUT_OPT_IN = "NO_MISTAKES_PI_STRUCTURED_OUTPUT";
const STRICT_RESPONSES_OPT_IN = "NO_MISTAKES_PI_STRICT_RESPONSES";
const STRICT_PROVIDER = "no-mistakes-flash-next-responses";
const STRICT_MODEL = "Qwen/Qwen3.8-Flash-Next-FP8";
const MAX_SCHEMA_BYTES = 1024 * 1024;
const MAX_PROVIDER_REQUESTS = 3;
const MAX_REPAIR_NUDGES = 2;
const REPAIR_NUDGES = Object.freeze({
  missing:
    "FORMAT_REPAIR_REQUIRED: No completed structured_output call was received. Do not repeat analysis. Do not output prose. Call structured_output exactly once with every required field.",
  schema:
    "FORMAT_REPAIR_REQUIRED: The structured_output arguments did not validate against the required schema. Do not repeat analysis. Do not output prose. Call structured_output exactly once with every required field and valid field types.",
});

function loadGateSchema(env = process.env) {
  if (typeof process.getuid !== "function") return undefined;
  if (env[GATE_MARKER] !== "1") return undefined;
  if (env[STRUCTURED_OUTPUT_OPT_IN] !== "1") return undefined;

  const path = env[SCHEMA_PATH];
  const expectedDigest = env[SCHEMA_DIGEST];
  if (!path || !isAbsolute(path) || !/^[a-f0-9]{64}$/.test(expectedDigest ?? "")) return undefined;

  let fd;
  try {
    fd = openSync(path, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
    const stat = fstatSync(fd);
    if (!stat.isFile() || stat.size <= 0 || stat.size > MAX_SCHEMA_BYTES) return undefined;
    if (stat.uid !== process.getuid() || (stat.mode & 0o077) !== 0) return undefined;

    const bounded = Buffer.alloc(stat.size + 1);
    let bytesRead = 0;
    while (bytesRead < bounded.length) {
      const count = readSync(fd, bounded, bytesRead, bounded.length - bytesRead, bytesRead);
      if (count === 0) break;
      bytesRead += count;
    }
    if (bytesRead !== stat.size) return undefined;
    const bytes = bounded.subarray(0, bytesRead);
    const actualDigest = createHash("sha256").update(bytes).digest();
    if (!timingSafeEqual(actualDigest, Buffer.from(expectedDigest, "hex"))) return undefined;

    const schema = JSON.parse(bytes.toString("utf8"));
    if (schema === null || typeof schema !== "object" || Array.isArray(schema)) return undefined;
    if (schema.type !== "object") return undefined;
    return schema;
  } catch {
    return undefined;
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

async function loadTypeBoxCompile() {
  const typebox = await import("./typebox-compiler.mjs");
  return typebox.Compile;
}

async function acceptedGateSchema(env, loadCompile = loadTypeBoxCompile) {
  const schema = loadGateSchema(env);
  if (!schema) return undefined;
  let Compile;
  try {
    Compile = await loadCompile();
  } catch (error) {
    throw new Error(
      "no-mistakes structured output dependencies are missing; run `npm ci --prefix integrations/pi --ignore-scripts` from the trusted checkout",
      { cause: error },
    );
  }
  try {
    const metaValidator = Compile(draft07MetaSchema);
    if (!metaValidator.Check(schema)) return undefined;
    Compile(schema);
    return schema;
  } catch {
    return undefined;
  }
}

export default async function structuredOutputExtension(pi, options = {}) {
  const sourceEnv = options.env ?? process.env;
  const env = { ...sourceEnv };
  delete sourceEnv[SCHEMA_PATH];
  delete sourceEnv[SCHEMA_DIGEST];
  delete sourceEnv[STRUCTURED_OUTPUT_OPT_IN];
  delete sourceEnv[STRICT_RESPONSES_OPT_IN];
  const schema = await acceptedGateSchema(
    env,
    options.loadCompile,
  );
  if (!schema) return;

  const strictResponsesValue = env[STRICT_RESPONSES_OPT_IN];
  if (strictResponsesValue !== undefined && strictResponsesValue !== "1") {
    throw new Error(`${STRICT_RESPONSES_OPT_IN} accepts only 1`);
  }
  const strictResponses = strictResponsesValue === "1";

  let strictRepair;
  let schemaValidator;
  if (strictResponses) {
    const Compile = await (options.loadCompile ?? loadTypeBoxCompile)();
    schemaValidator = Compile(schema);
    strictRepair = createStrictRepairController(pi, schemaValidator);
  }

  pi.registerTool({
    name: "structured_output",
    label: "Structured Output",
    description:
      "Return the final no-mistakes result. Call this exactly once as the last action after completing the requested work.",
    promptSnippet: "Emit the final no-mistakes result with the invocation's required JSON schema",
    promptGuidelines: [
      "Use structured_output exactly once as the final action after completing the no-mistakes task; do not return the final result as prose.",
    ],
    parameters: schema,
    ...(strictResponses
      ? { constrainedSampling: { type: "json_schema", strict: "require" } }
      : {}),
    async execute(_toolCallId, params) {
      if (strictRepair) strictRepair.accept(params);
      return {
        content: [{ type: "text", text: JSON.stringify(params) }],
        details: {},
        terminate: true,
      };
    },
  });

  if (strictResponses) {
    pi.on("message_end", (event) => strictRepair.observeMessage(event.message));
    pi.on("turn_end", (event) => strictRepair.finishTurn(event));
    pi.on("before_provider_request", (event, ctx) => {
      strictRepair.beginProviderRequest();
      return enforceStrictResponsesRequest(event.payload, ctx, pi);
    });
  }
}

function createStrictRepairController(pi, schemaValidator) {
  let providerRequests = 0;
  let repairNudges = 0;
  let completed = false;
  let turnDefect;
  let hardStop;

  const stop = (message) => {
    hardStop ??= new Error(message);
    throw hardStop;
  };

  return {
    beginProviderRequest() {
      if (hardStop) throw hardStop;
      providerRequests++;
      turnDefect = undefined;
      if (providerRequests > MAX_PROVIDER_REQUESTS) {
        throw new Error("strict no-mistakes gate exhausted its provider-request cap");
      }
    },
    observeMessage(message) {
      if (message?.role !== "assistant" || !Array.isArray(message.content)) return;
      const calls = message.content.filter((part) => part?.type === "toolCall");
      if (calls.length > 1) {
        stop("strict no-mistakes gate received multiple final tool calls");
      }
      if (calls.length === 0) return;
      const call = calls[0];
      if (call.name !== "structured_output") {
        stop("strict no-mistakes gate received a competing final tool call");
      }
      if (!schemaValidator.Check(call.arguments)) turnDefect = "schema";
    },
    accept(params) {
      if (hardStop) throw hardStop;
      if (completed) throw new Error("strict no-mistakes gate received multiple completed structured_output calls");
      if (!schemaValidator.Check(params)) {
        turnDefect = "schema";
        throw new Error("structured_output arguments do not validate against the required schema");
      }
      completed = true;
    },
    finishTurn(event) {
      if (hardStop) throw hardStop;
      if (completed) return;
      const message = event?.message;
      if (message?.role !== "assistant") {
        stop("strict no-mistakes gate reached an invalid terminal transport boundary");
      }
      // Provider, timeout, quota, auth, cancellation, and transport failures are
      // terminal. Never convert them into a model-format repair request.
      if (message.stopReason === "error" || message.errorMessage || message.error) return;
      const defect = turnDefect ?? "missing";
      if (repairNudges >= MAX_REPAIR_NUDGES) {
        throw new Error("strict no-mistakes gate exhausted two same-session format repairs");
      }
      repairNudges++;
      pi.sendUserMessage(REPAIR_NUDGES[defect], {
        deliverAs: "followUp",
        triggerTurn: true,
      });
    },
    state() {
      return { providerRequests, repairNudges, completed };
    },
  };
}

function enforceStrictResponsesRequest(payload, ctx, pi) {
  const model = ctx?.model;
  if (model?.provider !== STRICT_PROVIDER || model?.id !== STRICT_MODEL || model?.api !== "openai-responses") {
    throw new Error(
      `strict no-mistakes gate requires ${STRICT_PROVIDER}/${STRICT_MODEL} over openai-responses`,
    );
  }
  if (ctx?.thinkingLevel !== "xhigh") {
    throw new Error("strict no-mistakes gate requires xhigh reasoning");
  }
  if (typeof pi.getActiveTools !== "function" ||
      JSON.stringify(pi.getActiveTools()) !== JSON.stringify(["structured_output"])) {
    throw new Error("strict no-mistakes gate requires structured_output as its only active tool");
  }
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    throw new Error("strict no-mistakes gate received a malformed provider payload");
  }
  if (payload.model !== STRICT_MODEL || !Array.isArray(payload.input) || payload.stream !== true) {
    throw new Error("strict no-mistakes gate received an inconsistent Responses payload");
  }
  if (!Array.isArray(payload.tools) || payload.tools.length !== 1) {
    throw new Error("strict no-mistakes gate requires exactly one serialized tool");
  }
  const tool = payload.tools[0];
  if (tool?.type !== "function" || tool?.name !== "structured_output" || tool?.strict !== true ||
      tool?.parameters === null || typeof tool?.parameters !== "object") {
    throw new Error("strict no-mistakes gate requires a strict structured_output function");
  }
  if (payload.reasoning?.effort !== "xhigh") {
    throw new Error("strict no-mistakes gate payload lost xhigh reasoning");
  }

  const result = {
    ...payload,
    tool_choice: "required",
    parallel_tool_calls: false,
    include_reasoning: false,
  };
  // vLLM's Responses surface uses include_reasoning rather than OpenAI's
  // encrypted reasoning include selector. Keep private reasoning off the wire
  // response while retaining its token usage.
  delete result.include;
  return result;
}

export {
  MAX_PROVIDER_REQUESTS,
  MAX_REPAIR_NUDGES,
  MAX_SCHEMA_BYTES,
  REPAIR_NUDGES,
  STRICT_MODEL,
  STRICT_PROVIDER,
  acceptedGateSchema,
  createStrictRepairController,
  enforceStrictResponsesRequest,
  loadGateSchema,
};
