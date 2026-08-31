# no-mistakes structured output for ACP Pi

This Pi package provides the gate-scoped `structured_output` tool used by the no-mistakes ACP adapter. It consumes only the invocation-scoped, owner-only schema transport created by `internal/agent/acpx.go`; `NO_MISTAKES_GATE=1` is a required containment marker, not authorization.

See the [agent guide](../../docs/src/content/docs/guides/agents.md#exact-structured-output-with-acp-pi) for package and ACP-only wrapper setup. Choose one setup, not both.

The extension contract tests cover inactive ordinary sessions, transport refusal, exact review/test-shaped schemas, exact JSON result text, and termination. `internal/agent/pi_structured_output_extension_test.go` also starts a real Pi RPC process through the wrapper to prove Pi accepts the transported schema. A live model tool-choice test is intentionally not part of the credential-neutral suite; final model output remains covered by no-mistakes' adapter-side JSON Schema validation.
