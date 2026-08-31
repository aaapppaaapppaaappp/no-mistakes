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
  printf '%s\n%s\n%s\n' "$NO_MISTAKES_GATE" "$NO_MISTAKES_JSON_SCHEMA_FILE" "$NO_MISTAKES_JSON_SCHEMA_SHA256" > "$NM_TEST_ACPX_ENV_FILE"
fi
if [ -n "$NM_TEST_ACPX_SCHEMA_COPY" ] && [ -n "$NO_MISTAKES_JSON_SCHEMA_FILE" ]; then
  cat "$NO_MISTAKES_JSON_SCHEMA_FILE" > "$NM_TEST_ACPX_SCHEMA_COPY"
fi
if [ -n "$NM_TEST_ACPX_EVENT" ]; then
  printf '%s\n' "$NM_TEST_ACPX_EVENT"
elif [ -n "$NO_MISTAKES_JSON_SCHEMA_FILE" ]; then
  printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"{\\"artifacts\\":[]}"}}}\n'
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
	a := &acpxAgent{bin: stub, target: "pi"}
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
	if len(envLines) != 3 || envLines[0] != "1" || envLines[1] == "" || len(envLines[2]) != 64 {
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

// TestAcpxAgent_Run_CursorSpawnsDefaultCommandWithoutOverrides proves both
// spellings of the Cursor agent drive a real acpx spawn with the alias
// default raw command — no acp_registry_overrides entry configured.
func TestAcpxAgent_Run_CursorSpawnsDefaultCommandWithoutOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent types.AgentName
	}{
		{name: "cursor alias", agent: types.AgentCursor},
		{name: "explicit acp:cursor target", agent: "acp:cursor"},
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
			if string(envData) != "1\n\n\n" {
				t.Errorf("unstructured child gate/schema environment = %q, want gate marker and cleared schema transport", envData)
			}

			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("stub acpx never recorded argv: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(argv) < 2 || argv[0] != "--agent" || argv[1] != "cursor-agent acp" {
				t.Errorf("spawned argv = %q, want leading --agent \"cursor-agent acp\"", argv)
			}
			if len(argv) < 3 || strings.Join(argv[len(argv)-3:], "\x00") != "exec\x00--file\x00-" {
				t.Errorf("spawned argv = %q, want trailing exec --file -", argv)
			}
			for _, arg := range argv {
				if arg == "cursor" {
					t.Errorf("spawned argv = %q, must not pass the bare target when the default command is supplied", argv)
				}
			}
			t.Logf("spawned: acpx %s", strings.Join(argv, " "))
		})
	}
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
