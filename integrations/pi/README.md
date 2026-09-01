# no-mistakes structured output for ACP Pi

This Ubuntu-only Pi package provides the gate-scoped `structured_output` tool used by the no-mistakes ACP adapter. It consumes only the invocation-scoped, owner-only schema transport created by `internal/agent/acpx.go`; `NO_MISTAKES_GATE=1` is a required containment marker, not authorization.

See the [global configuration reference](../../docs/src/content/docs/reference/global-config.md#acp_registry_overrides) for ACP-only wrapper setup.

## Strict Flash-Next Responses gate

`bin/pi-no-mistakes-flash-next-responses-acp` is the one narrow required+strict route. It pins provider `no-mistakes-flash-next-responses`, model `Qwen/Qwen3.8-Flash-Next-FP8`, API `openai-responses`, and reasoning `xhigh`; disables discovered extensions, built-in tools, project settings/context, sessions, and startup network work; and leaves only `structured_output` active. It uses an owner-only temporary Pi directory whose `models.json` and `auth.json` are symlinks to the operator-owned files, so it neither copies credentials nor changes authentication.

After the existing gate marker, opt-in, owner/mode, exact schema bytes, digest, and Draft-07 checks pass, the extension:

- requires Pi/pi-ai JSON-Schema constrained sampling (`strict: true`);
- verifies the exact provider/model/API/xhigh and one-tool payload;
- forces Responses `tool_choice: "required"` and `parallel_tool_calls: false`;
- suppresses returned private reasoning while retaining usage accounting.

The hook throws rather than mutating any ordinary Pi, Chat Completions, other-model, other-provider, multi-tool, or untrusted turn. `NO_MISTAKES_PI_STRICT_RESPONSES=1` is wrapper-internal and is consumed before child tools run.

The committed fixtures are exact but inactive:

- `fixtures/flash-next-responses-models.json` defines the credential-neutral provider/model/API/strict capability/xhigh/sampling contract.
- `fixtures/flash-next-responses-route.yaml` defines the separately named ACP target and explicit one-ACP-attempt control.

Merge the provider entry into a disposable or operator-owned Pi catalogue and replace its loopback base URL only with a currently healthy, independently identity-pinned route. The loopback value is an example, not evidence that the historically stale Flash-Next tunnel is healthy. Do not copy the fixtures into machine-global production or select the target merely because tests pass; activation remains a separate operator decision.

In exact-output mode no-mistakes accepts one completed `structured_output` call only. Its arguments are the sole authoritative result: assistant prose from the same attempt is discarded, cannot override or supplement fields, is never streamed as the final result, and records only a bounded lifecycle warning. Zero or duplicate calls, a wrong/coissued tool, failed/incomplete calls, invalid terminal activity, malformed events, and final-schema-invalid arguments fail with a typed protocol error and produce no `Result`; prose without a valid call therefore still fails closed. The fixed pi-acp startup banner is adapter metadata, not assistant prose. Final no-mistakes JSON-Schema validation remains authoritative.

`NO_MISTAKES_ACPX_ATTEMPTS=1` disables no-mistakes retries and makes acpx's prompt retry count explicitly zero. The dedicated wrapper also disables Pi agent and provider retries. Inside that one Pi process and persistent ACP/Pi session, the gate extension may issue at most two fixed, content-free format-repair nudges after a settled turn has no call or one recoverable schema/field/tool-name defect. This bounds the invocation to one ACP prompt and at most three provider requests. A nudge names only the objective validation defect; it never quotes assistant prose, private reasoning, or mutable model content. Multiple calls, competing activity, route drift, transport/provider failures, or exhaustion terminate rather than cold-retry. Values other than `1` are refused. Every other ACP target retains its existing retry behavior.

The extension unit tests cover inactive/untrusted sessions, exact schema registration, strict request mutation/refusal, exact JSON result text, and termination. `internal/e2e/pi_structured_output_test.go` runs credential-neutral process tests through acpx, pinned pi-acp, the dedicated wrapper, Pi, and a synthetic Responses provider, asserting request bytes and one process/request/attempt. Live model validation is deliberately separate and bounded.
