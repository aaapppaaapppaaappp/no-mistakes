//go:build unix

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStubAcpx writes a stub acpx binary that records its argv (one arg per
// line) and, when requested, stdin and the invocation's structured-output
// transport before the adapter cleans it up. It then emits a minimal valid
// acpx JSON event stream.
func writeStubAcpx(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
if [ -n "$NM_TEST_ACPX_STDIN_FILE" ]; then
  cat > "$NM_TEST_ACPX_STDIN_FILE"
else
  cat > /dev/null
fi
if [ -n "$NM_TEST_ACPX_ENV_FILE" ]; then
  printf '%s\n%s\n%s\n%s\n' "$NO_MISTAKES_GATE" "$NO_MISTAKES_JSON_SCHEMA_FILE" "$NO_MISTAKES_JSON_SCHEMA_SHA256" "$NO_MISTAKES_PI_STRUCTURED_OUTPUT" > "$NM_TEST_ACPX_ENV_FILE"
fi
if [ -n "$NM_TEST_ACPX_SCHEMA_COPY" ] && [ -n "$NO_MISTAKES_JSON_SCHEMA_FILE" ]; then
  cat "$NO_MISTAKES_JSON_SCHEMA_FILE" > "$NM_TEST_ACPX_SCHEMA_COPY"
fi
if [ -n "$NM_TEST_ACPX_EVENT" ]; then
  printf '%s\n' "$NM_TEST_ACPX_EVENT"
elif [ -n "$NO_MISTAKES_JSON_SCHEMA_FILE" ]; then
  printf '%s\n' '{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"structured-1","title":"structured_output","status":"in_progress"}}}'
  printf '%s\n' '{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"structured-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"{\"artifacts\":[]}"}}]}}}'
else
  printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"cursor stub reply"}}}\n'
fi
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAcpxAgent_Run_TransportsExactSchemaAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.txt")
	envFile := filepath.Join(dir, "env.txt")
	schemaCopy := filepath.Join(dir, "schema.json")
	stdinFile := filepath.Join(dir, "stdin.txt")
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
	t.Setenv("NM_TEST_ACPX_ENV_FILE", envFile)
	t.Setenv("NM_TEST_ACPX_SCHEMA_COPY", schemaCopy)
	t.Setenv("NM_TEST_ACPX_STDIN_FILE", stdinFile)
	stub := writeStubAcpx(t, dir)

	schema := json.RawMessage(`{"type":"object","properties":{"artifacts":{"type":"array","items":{"type":"object","required":["label"]}},"risk_scope":{"type":"string","enum":["source-or-external","pipeline-owned-delivery"]}},"required":["artifacts"]}`)
	a := &acpxAgent{bin: stub, target: "pi", rawCommand: "env NO_MISTAKES_PI_STRUCTURED_OUTPUT=1 pi-acp"}
	res, err := a.Run(context.Background(), RunOpts{Prompt: "test", CWD: dir, JSONSchema: schema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != `{"artifacts":[]}` {
		t.Fatalf("result text = %q", res.Text)
	}

	copied, err := os.ReadFile(schemaCopy)
	if err != nil {
		t.Fatalf("read transported schema: %v", err)
	}
	if string(copied) != string(schema) {
		t.Errorf("transported schema = %s, want exact %s", copied, schema)
	}
	envData, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read child env: %v", err)
	}
	envLines := strings.Split(strings.TrimSpace(string(envData)), "\n")
	if len(envLines) != 4 || envLines[0] != "1" || envLines[1] == "" || len(envLines[2]) != 64 || envLines[3] != "1" {
		t.Fatalf("child gate/schema environment = %q", envLines)
	}
	sum := sha256.Sum256(schema)
	if envLines[2] != hex.EncodeToString(sum[:]) {
		t.Errorf("child schema digest = %q, want digest of exact schema", envLines[2])
	}
	if _, err := os.Stat(envLines[1]); !os.IsNotExist(err) {
		t.Errorf("schema transport still exists after Run: %v", err)
	}
	stdinData, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdinData), string(schema)) {
		t.Errorf("existing structured prompt contract lost exact schema: %s", stdinData)
	}
	argvData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argvData), envLines[1]) {
		t.Errorf("schema transport path leaked into child arguments: %s", argvData)
	}
}

func TestCreateACPXSchemaTransport_UsesAbsolutePathWithRelativeTMPDIR(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	relativeTempDir, err := filepath.Rel(cwd, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", relativeTempDir)

	path, cleanup, err := createACPXSchemaTransport(json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`))
	if err != nil {
		t.Fatalf("create schema transport: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("schema transport path = %q, want absolute path", path)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("schema transport still exists after cleanup: %v", err)
	}
}

func TestAcpxAgent_Run_RefusesInvalidStructuredOutputTransport(t *testing.T) {
	dir := t.TempDir()
	stub := writeStubAcpx(t, dir)
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", filepath.Join(dir, "argv.txt"))

	for _, tc := range []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "malformed", schema: json.RawMessage(`{"type":`)},
		{name: "non-object root", schema: json.RawMessage(`[]`)},
		{name: "non-object parameters", schema: json.RawMessage(`{"type":"array"}`)},
		{name: "invalid required keyword", schema: json.RawMessage(`{"type":"object","required":"summary"}`)},
		{name: "oversized", schema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", acpxSchemaMaxBytes) + `"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &acpxAgent{bin: stub, target: "pi"}
			_, err := a.Run(context.Background(), RunOpts{Prompt: "test", CWD: dir, JSONSchema: tc.schema})
			if err == nil || !strings.Contains(err.Error(), "schema transport") {
				t.Fatalf("Run error = %v, want schema transport refusal", err)
			}
		})
	}
}

// TestAcpxAgent_Run_AliasSpawnsDefaultCommandWithoutOverrides proves both
// spellings of every first-class ACP alias drive a real acpx spawn with the
// alias default raw command - no acp_registry_overrides entry configured.
func TestAcpxAgent_Run_AliasSpawnsDefaultCommandWithoutOverrides(t *testing.T) {
	for _, tc := range []struct {
		name    string
		agent   types.AgentName
		target  string
		command string
	}{
		{name: "cursor alias", agent: types.AgentCursor, target: "cursor", command: "cursor-agent acp"},
		{name: "explicit acp:cursor target", agent: "acp:cursor", target: "cursor", command: "cursor-agent acp"},
		{name: "devin alias", agent: types.AgentDevin, target: "devin", command: "devin acp"},
		{name: "explicit acp:devin target", agent: "acp:devin", target: "devin", command: "devin acp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			envFile := filepath.Join(dir, "env.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_STDIN_FILE", filepath.Join(dir, "stdin.txt"))
			t.Setenv("NM_TEST_ACPX_ENV_FILE", envFile)
			t.Setenv(acpxSchemaEnvVar, "/tmp/ambient-stale-schema.json")
			t.Setenv(acpxSchemaDigestEnvVar, strings.Repeat("a", 64))
			stub := writeStubAcpx(t, dir)

			a, err := New(tc.agent, stub, nil)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.agent, err)
			}
			if a.Name() != "acp:"+tc.target {
				t.Errorf("Name() = %q, want acp:%s", a.Name(), tc.target)
			}
			res, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Text != "cursor stub reply" {
				t.Errorf("result text = %q, want stub acpx output", res.Text)
			}

			envData, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatalf("stub acpx never recorded env: %v", err)
			}
			if string(envData) != "1\n\n\n\n" {
				t.Errorf("unstructured child gate/schema environment = %q, want gate marker and cleared schema transport", envData)
			}

			argv := readStubAcpxArgv(t, argsFile)
			if len(argv) < 2 || argv[0] != "--agent" || argv[1] != tc.command {
				t.Errorf("spawned argv = %q, want leading --agent %q", argv, tc.command)
			}
			if len(argv) < 3 || strings.Join(argv[len(argv)-3:], "\x00") != "exec\x00--file\x00-" {
				t.Errorf("spawned argv = %q, want trailing exec --file -", argv)
			}
			for _, arg := range argv {
				if arg == tc.target {
					t.Errorf("spawned argv = %q, must not pass the bare target when the default command is supplied", argv)
				}
			}
			t.Logf("spawned: acpx %s", strings.Join(argv, " "))
		})
	}
}

// TestAcpxAgent_Run_DevinOverrideAndModelReachTheSpawn proves an operator's
// acp_registry_overrides.devin replaces the `devin acp` default in the real
// spawn, and an agent_config model rides acpx's own --model ahead of exec.
func TestAcpxAgent_Run_DevinOverrideAndModelReachTheSpawn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		overrides   map[string]string
		model       string
		wantCommand string
	}{
		{name: "override wins", overrides: map[string]string{"devin": "devin acp --model claude-opus-5-5-high"}, wantCommand: "devin acp --model claude-opus-5-5-high"},
		{name: "blank override keeps default", overrides: map[string]string{"devin": " \t"}, wantCommand: "devin acp"},
		{name: "model pin", model: "gpt-6-luna-medium", wantCommand: "devin acp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_STDIN_FILE", filepath.Join(dir, "stdin.txt"))

			a, err := NewWithOptions(types.AgentDevin, writeStubAcpx(t, dir), nil, Options{
				ACPRegistryOverrides: tc.overrides,
				Profile:              agentcfg.Profile{Model: tc.model},
			})
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			if _, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			argv := readStubAcpxArgv(t, argsFile)
			if len(argv) < 2 || argv[0] != "--agent" || argv[1] != tc.wantCommand {
				t.Fatalf("spawned argv = %q, want leading --agent %q", argv, tc.wantCommand)
			}
			modelAt := -1
			for i, arg := range argv {
				if arg == "--model" && i > 1 {
					modelAt = i
				}
			}
			if tc.model == "" {
				if modelAt >= 0 {
					t.Fatalf("spawned argv = %q, want no acpx --model without a pin", argv)
				}
				return
			}
			if modelAt < 0 || modelAt+1 >= len(argv) || argv[modelAt+1] != tc.model {
				t.Fatalf("spawned argv = %q, want acpx --model %s", argv, tc.model)
			}
			if execAt := len(argv) - 3; modelAt > execAt {
				t.Fatalf("spawned argv = %q, want --model ahead of exec", argv)
			}
		})
	}
}

// TestAcpxAgent_Run_DevinObservedStreamYieldsStructuredOutputAndUsage replays
// the event shape a live `devin acp` turn produced through acpx 0.13.0: thought
// chunks, a permission-gated shell tool call, usage_update events whose token
// counts sit under cognition.ai-namespaced _meta keys, a JSON-only final message
// split across agent_message_chunk events, and camelCase result.usage.
func TestAcpxAgent_Run_DevinObservedStreamYieldsStructuredOutputAndUsage(t *testing.T) {
	events := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Planning"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","kind":"execute","title":"Ran printf, test","rawInput":{"command":"printf hi > hello.txt"}}}}`,
		`{"jsonrpc":"2.0","id":"p1","method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"c1"},"options":[{"optionId":"allow_once","name":"Allow","kind":"allow_once"}]}}`,
		`{"jsonrpc":"2.0","id":"p1","result":{"outcome":{"outcome":"selected","optionId":"allow_once"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":13554,"size":1000000,"_meta":{"cognition.ai/inputTokens":13518,"cognition.ai/outputTokens":36}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"{\"shell_output\":\"true\","}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"\"file_created\":true}"}}}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","usage":{"totalTokens":13554,"inputTokens":13518,"outputTokens":36,"cachedReadTokens":13345,"cachedWriteTokens":170}}}`,
	}, "\n")
	dir := t.TempDir()
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", filepath.Join(dir, "argv.txt"))
	t.Setenv("NM_TEST_ACPX_STDIN_FILE", filepath.Join(dir, "stdin.txt"))
	t.Setenv("NM_TEST_ACPX_EVENT", events)

	a, err := New(types.AgentDevin, writeStubAcpx(t, dir), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var chunks []string
	res, err := a.Run(context.Background(), RunOpts{
		Prompt:     "run a command and create a file",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"shell_output":{"type":"string"},"file_created":{"type":"boolean"}},"required":["shell_output","file_created"]}`),
		OnChunk:    func(chunk string) { chunks = append(chunks, chunk) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(chunks) != 2 {
		t.Errorf("streamed chunks = %q, want the two agent_message_chunk texts and no thought text", chunks)
	}
	var out struct {
		ShellOutput string `json:"shell_output"`
		FileCreated bool   `json:"file_created"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.ShellOutput != "true" || !out.FileCreated {
		t.Fatalf("structured output = %s (%v), want the JSON-only final message", res.Output, err)
	}
	if !res.UsageReported {
		t.Fatal("Devin reports usage over ACP; the result must record it as reported")
	}
	if res.Usage.OutputTokens != 36 || res.Usage.CacheReadTokens != 13345 || res.Usage.CacheCreationTokens != 170 {
		t.Errorf("usage = %+v, want result.usage output/cache counts", res.Usage)
	}
}

func readStubAcpxArgv(t *testing.T, argsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("stub acpx never recorded argv: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestAcpxAgent_Run_SendsLargePromptOnlyOnStdin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "plain prompt"},
		{name: "structured prompt", schema: json.RawMessage(`{"type":"object"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			stdinFile := filepath.Join(dir, "stdin.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_STDIN_FILE", stdinFile)

			prompt := strings.Repeat("x", 4096)
			wantPrompt := prompt
			if len(tc.schema) > 0 {
				wantPrompt = buildACPStructuredPrompt(prompt, tc.schema)
				t.Setenv("NM_TEST_ACPX_EVENT", `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"{\"ok\":true}"}}}`)
			}
			a := &acpxAgent{bin: writeStubAcpx(t, dir), target: "gemini"}
			if _, err := a.Run(context.Background(), RunOpts{Prompt: prompt, CWD: dir, JSONSchema: tc.schema}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			argsData, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read argv: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(argsData), "\n"), "\n")
			if len(argv) < 3 || strings.Join(argv[len(argv)-3:], "\x00") != "exec\x00--file\x00-" {
				t.Fatalf("spawned argv = %q, want trailing exec --file -", argv)
			}
			for _, arg := range argv {
				if arg == wantPrompt {
					t.Fatalf("spawned argv contains the prompt")
				}
			}

			stdinData, err := os.ReadFile(stdinFile)
			if err != nil {
				t.Fatalf("read stdin: %v", err)
			}
			if got := string(stdinData); got != wantPrompt {
				t.Fatalf("stdin prompt mismatch: got %d bytes, want %d", len(got), len(wantPrompt))
			}
		})
	}
}

func TestAcpxAgent_Run_SurfacesStdinWriteFailure(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"early reply"}}}\n'
printf 'acpx: unknown option --file\n' >&2
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &acpxAgent{bin: stub, target: "gemini"}
	_, err := a.Run(ctx, RunOpts{Prompt: strings.Repeat("x", 2*1024*1024), CWD: dir})
	if err == nil || !strings.Contains(err.Error(), "acpx stdin") {
		t.Fatalf("Run error = %v, want acpx stdin write failure", err)
	}
	if !strings.Contains(err.Error(), "unknown option --file") {
		t.Fatalf("Run error = %v, want child stderr in stdin write failure", err)
	}
}

// TestAcpxAgent_Run_FailedTurnEstimatesTheOutputItStreamed proves a turn that
// reports input-only usage, streams an answer, and then fails does not record
// its output as a reported zero: the estimate the success path applies is
// applied on the failed path too. acpx reports usage per event, so a failed
// turn routinely carries a real input count with no output count, and
// resultFromUsage would otherwise hand instrumentation Reported=true with
// OutputTokens=0 - a fabricated zero rather than an unknown.
func TestAcpxAgent_Run_FailedTurnEstimatesTheOutputItStreamed(t *testing.T) {
	const streamed = "partial answer before acpx died"

	dir := t.TempDir()
	stub := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
cat > /dev/null
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","input_tokens":1200}}}\n'
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"` + streamed + `"}}}\n'
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a, err := New(types.AgentCursor, stub, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir})
	if err == nil {
		t.Fatal("expected the non-zero exit to fail the turn")
	}
	if res == nil {
		t.Fatal("a failed turn that reported usage must still return it")
	}
	if !res.UsageReported || res.Usage.InputTokens != 1200 {
		t.Fatalf("usage = %+v reported=%v, want the reported input count", res.Usage, res.UsageReported)
	}
	if want := estimateAcpxTokens(len(streamed)); res.Usage.OutputTokens != want {
		t.Errorf("output tokens = %d, want the %d-token estimate of the text acpx streamed", res.Usage.OutputTokens, want)
	}
}
