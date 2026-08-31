//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
