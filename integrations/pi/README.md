# no-mistakes structured output for ACP Pi

This Pi package provides the gate-scoped `structured_output` tool used by the no-mistakes ACP adapter. It consumes only the invocation-scoped, owner-only schema transport created by `internal/agent/acpx.go`; `NO_MISTAKES_GATE=1` is a required containment marker, not authorization.

The extension is intentionally inactive on Windows because Node does not expose the POSIX ownership and mode checks used to authenticate the schema file. Windows ACP/Pi sessions receive no `structured_output` tool or prompt metadata; ordinary Pi behavior is unchanged.

See the [agent guide](../../docs/src/content/docs/guides/agents.md#exact-structured-output-with-acp-pi) for package and ACP-only wrapper setup. Choose one setup, not both.

The extension contract tests cover inactive ordinary sessions, transport refusal, exact review/test-shaped schemas, exact JSON result text, and termination. `internal/agent/pi_structured_output_extension_test.go` also runs a credential-neutral fixture provider through acpx, pi-acp, and Pi to prove the terminating tool result reaches no-mistakes' final schema validation. A live model tool-choice test is intentionally not part of the credential-neutral suite.
