package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func piStructuredOutputIntegrationPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "integrations", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPiStructuredOutputExtensionContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the repository-owned Pi extension contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join(piStructuredOutputIntegrationPath(t), "test", "structured-output.test.mjs"))
	shellenv.ConfigureShellCommand(cmd)
	if output, err := shellenv.CombinedOutputShellCommand(cmd); err != nil {
		t.Fatalf("Pi extension contract tests: %v\n%s", err, output)
	}
}

func TestAcpxPiStructuredOutputTerminatingToolIntegration(t *testing.T) {
	acpx, err := exec.LookPath("acpx")
	if err != nil {
		t.Skip("acpx is required for the ACP/Pi integration test")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is required for the ACP/Pi integration test")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx with cached pi-acp 0.0.31 is required for the ACP/Pi integration test")
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer probeCancel()
	probe := exec.CommandContext(probeCtx, npx, "--offline", "--yes", "pi-acp@0.0.31", "--help")
	shellenv.ConfigureShellCommand(probe)
	if output, err := shellenv.CombinedOutputShellCommand(probe); err != nil {
		t.Skipf("cached pi-acp 0.0.31 is unavailable: %v: %s", err, output)
	}

	dir := t.TempDir()
	providerPath := filepath.Join(dir, "fixture-provider.mjs")
	provider := `import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";
export default function fixtureProvider(pi) {
  pi.registerProvider("no-mistakes-fixture", {
    baseUrl: "http://127.0.0.1",
    apiKey: "credential-neutral-fixture",
    api: "openai-completions",
    models: [{
      id: "structured-output",
      name: "Structured Output Fixture",
      reasoning: false,
      input: ["text"],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: 32000,
      maxTokens: 1024
    }],
    streamSimple(model) {
      const stream = createAssistantMessageEventStream();
      queueMicrotask(() => {
        const toolCall = {
          type: "toolCall",
          id: "structured-output-fixture-call",
          name: "structured_output",
          arguments: { summary: "through-acp" }
        };
        const message = {
          role: "assistant",
          content: [toolCall],
          api: model.api,
          provider: model.provider,
          model: model.id,
          usage: {
            input: 1,
            output: 1,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 2,
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 }
          },
          stopReason: "toolUse",
          timestamp: Date.now()
        };
        stream.push({ type: "start", partial: message });
        stream.push({ type: "toolcall_start", contentIndex: 0, partial: message });
        stream.push({ type: "toolcall_end", contentIndex: 0, toolCall, partial: message });
        stream.push({ type: "done", reason: "toolUse", message });
        stream.end();
      });
      return stream;
    }
  });
}`
	if err := os.WriteFile(providerPath, []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}

	wrapperPath := filepath.Join(piStructuredOutputIntegrationPath(t), "bin", "pi-no-mistakes-acp")
	piCommand := fmt.Sprintf("%q -e %q --provider no-mistakes-fixture --model structured-output --no-session --no-themes --no-context-files --offline", wrapperPath, providerPath)
	rawCommand := fmt.Sprintf("env PI_ACP_PI_COMMAND=%q %q --offline --yes pi-acp@0.0.31", piCommand, npx)
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","enum":["through-acp"]}},"required":["summary"],"additionalProperties":false}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := &acpxAgent{bin: acpx, target: "pi", rawCommand: rawCommand}
	res, err := a.Run(ctx, RunOpts{
		Prompt:     "Return the fixture result",
		CWD:        dir,
		Env:        []string{"HOME=" + dir, "PI_CODING_AGENT_DIR=" + filepath.Join(dir, ".pi", "agent")},
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("ACP/Pi structured output integration: %v", err)
	}
	if res.Text != `{"summary":"through-acp"}` {
		t.Fatalf("result text = %q, want terminating tool JSON", res.Text)
	}
}

func TestPiStructuredOutputExtensionLoadsInPiRPC(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed; the extension contract test still covers registration and execution")
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	schema := []byte(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schema)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapperPath := filepath.Join(piStructuredOutputIntegrationPath(t), "bin", "pi-no-mistakes-acp")
	cmd := exec.CommandContext(ctx, wrapperPath,
		"--mode", "rpc",
		"--no-session",
		"--no-themes",
		"--no-context-files",
		"--offline",
	)
	cmd.Dir = dir
	cmd.Env = append(gitSafeEnv(dir),
		"HOME="+dir,
		acpxSchemaEnvVar+"="+schemaPath,
		acpxSchemaDigestEnvVar+"="+hex.EncodeToString(sum[:]),
	)
	cmd.Stdin = strings.NewReader("{\"id\":\"probe\",\"type\":\"get_state\"}\n")
	shellenv.ConfigureShellCommand(cmd)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("Pi RPC extension probe: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, `"id":"probe"`) || !strings.Contains(text, `"success":true`) {
		t.Fatalf("Pi RPC extension probe returned no successful response: %s", text)
	}
	if strings.Contains(text, "extension_error") {
		t.Fatalf("Pi rejected the transported schema extension: %s", text)
	}
}
