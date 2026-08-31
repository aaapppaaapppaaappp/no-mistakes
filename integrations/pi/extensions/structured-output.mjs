import { createHash, timingSafeEqual } from "node:crypto";
import { closeSync, constants, fstatSync, openSync, readSync } from "node:fs";
import { isAbsolute } from "node:path";

const GATE_MARKER = "NO_MISTAKES_GATE";
const SCHEMA_PATH = "NO_MISTAKES_JSON_SCHEMA_FILE";
const SCHEMA_DIGEST = "NO_MISTAKES_JSON_SCHEMA_SHA256";
const MAX_SCHEMA_BYTES = 1024 * 1024;

function loadGateSchema(env = process.env) {
  if (env[GATE_MARKER] !== "1") return undefined;

  const path = env[SCHEMA_PATH];
  const expectedDigest = env[SCHEMA_DIGEST];
  if (!path || !isAbsolute(path) || !/^[a-f0-9]{64}$/.test(expectedDigest ?? "")) return undefined;

  let fd;
  try {
    fd = openSync(path, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
    const stat = fstatSync(fd);
    if (!stat.isFile() || stat.size <= 0 || stat.size > MAX_SCHEMA_BYTES) return undefined;
    if (typeof process.getuid === "function") {
      if (stat.uid !== process.getuid() || (stat.mode & 0o077) !== 0) return undefined;
    }

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

export default function structuredOutputExtension(pi) {
  const schema = loadGateSchema();
  if (!schema) return;

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
    async execute(_toolCallId, params) {
      return {
        content: [{ type: "text", text: JSON.stringify(params) }],
        details: {},
        terminate: true,
      };
    },
  });
}

export { MAX_SCHEMA_BYTES, loadGateSchema };
