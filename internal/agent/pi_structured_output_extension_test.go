//go:build linux

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	integrationPath := piStructuredOutputIntegrationPath(t)
	dependencyPath := filepath.Join(integrationPath, "node_modules", "typebox", "package.json")
	if info, err := os.Stat(dependencyPath); err != nil || info.IsDir() {
		if os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1" {
			t.Fatalf("pinned dependency %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", dependencyPath)
		}
		t.Skipf("pinned dependency %s is required; run npm ci --prefix integrations/pi --ignore-scripts", dependencyPath)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the repository-owned Pi extension contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join(integrationPath, "test", "structured-output.test.mjs"))
	shellenv.ConfigureShellCommand(cmd)
	if output, err := shellenv.CombinedOutputShellCommand(cmd); err != nil {
		t.Fatalf("Pi extension contract tests: %v\n%s", err, output)
	}
}

func TestPiStructuredOutputExtensionLoadsInPiRPC(t *testing.T) {
	integrationPath := piStructuredOutputIntegrationPath(t)
	binDir := filepath.Join(integrationPath, "node_modules", ".bin")
	piPath := filepath.Join(binDir, "pi")
	if info, err := os.Stat(piPath); err != nil || info.IsDir() {
		if os.Getenv("NM_REQUIRE_PI_ACP_INTEGRATION") == "1" {
			t.Fatalf("pinned executable %s is missing; run npm ci --prefix integrations/pi --ignore-scripts", piPath)
		}
		t.Skipf("pinned executable %s is required; run npm ci --prefix integrations/pi --ignore-scripts", piPath)
	}

	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	extensionsDir := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionsDir, "stale-structured-output.mjs"), []byte(`throw new Error("stale profile extension loaded")`), 0o600); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(dir, "schema.json")
	schema := []byte(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schema)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapperPath := filepath.Join(integrationPath, "bin", "pi-no-mistakes-acp")
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
		"PI_CODING_AGENT_DIR="+agentDir,
		"NO_MISTAKES_GATE=1",
		acpxStructuredOutputEnvVar+"=1",
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
