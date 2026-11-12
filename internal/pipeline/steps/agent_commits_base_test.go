package steps

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentBaseMerge_UsesLiveBaseAcrossProvidersAndFixPaths(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TEA_CONFIG_DIR", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "hosts.yml"), []byte("hosts:\n  https://gitea.example: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		provider    scm.Provider
		remote, url string
		payload     string
	}{
		{scm.ProviderGitHub, "https://github.com/test/repo", "https://github.com/test/repo/pull/42",
			`[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"develop"}]`},
		{scm.ProviderGitLab, "https://gitlab.com/test/repo", "https://gitlab.com/test/repo/-/merge_requests/42",
			`[{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","target_branch":"develop"}]`},
		{scm.ProviderGitea, "https://gitea.example/test/repo", "https://gitea.example/test/repo/pulls/42",
			`[{"number":42,"url":"https://gitea.example/test/repo/pulls/42","state":"open","head":"feature","base":"develop"}]`},
		{scm.ProviderForgejo, "https://forge.example/test/repo", "https://forge.example/test/repo/pulls/42",
			`{"found":true,"pull_request":{"number":42,"url":"https://forge.example/test/repo/pulls/42","api_url":"https://forge.example/api/v1/repos/test/repo/pulls/42","state":"open","title":"Feature","head":"feature","base":"develop","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"search_info":{"complete":true,"pages":1,"fetched":1,"total":1}}`},
		{scm.ProviderAzureDevOps, "https://dev.azure.com/test/project/_git/repo", "https://dev.azure.com/test/project/_git/repo/pullrequest/42",
			`[{"pullRequestId":42,"targetRefName":"refs/heads/develop","sourceRefName":"refs/heads/feature","repository":{"name":"repo","project":{"name":"project"}}}]`},
		{scm.ProviderBitbucket, "https://bitbucket.org/test/repo", "https://bitbucket.org/test/repo/pull-requests/42",
			`{"values":[{"id":42,"state":"OPEN","destination":{"branch":{"name":"develop"}}}]}`},
	} {
		for _, persisted := range []bool{false, true} {
			for _, route := range []string{"shared", "ci", "retained"} {
				t.Run(fmt.Sprintf("%s/persisted=%t/%s", tc.provider, persisted, route), func(t *testing.T) {
					sctx, dir, head := agentCommitContext(t, nil)
					gitCmd(t, dir, "checkout", "-b", "develop", "main")
					agentCommit(t, dir, "develop.txt", "only on develop")
					tip := gitCmd(t, dir, "rev-parse", "HEAD")
					gitCmd(t, dir, "checkout", "--detach", head)
					gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "develop")
					wantTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
					sctx.Config.PR.BaseBranch = "main"
					sctx.Config.CI.RevalidateRepairs = true
					sctx.Repo.UpstreamURL = tc.remote
					sctx.ForgeContext = &forgecontext.Context{Provider: tc.provider}
					if persisted {
						sctx.Run.PRURL = strptr(tc.url)
					}
					if tc.provider == scm.ProviderBitbucket {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodGet || r.URL.Path != "/2.0/repositories/test/repo/pullrequests" ||
								!strings.Contains(r.URL.Query().Get("q"), `source.branch.name="feature"`) ||
								strings.Contains(r.URL.Query().Get("q"), "destination.branch") {
								http.Error(w, "expected branch-only PR discovery", http.StatusBadRequest)
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(tc.payload))
						}))
						t.Cleanup(server.Close)
						sctx.Env = []string{"NO_MISTAKES_BITBUCKET_EMAIL=test@example.com", "NO_MISTAKES_BITBUCKET_API_TOKEN=test", "NO_MISTAKES_BITBUCKET_API_BASE_URL=" + server.URL}
					} else {
						binDir := fakeCLIBinDir(t)
						linkTestBinary(t, binDir, tc.provider.CLIName())
						sctx.Env = fakeCLIEnv(binDir, map[string]string{
							"FAKE_CLI_MODE": "pr-discovery", "FAKE_CLI_PR_LIST_JSON": tc.payload,
							"FORGEJO_BASE_URL": "", "FORGEJO_SSH_DOMAIN": "",
						})
					}
					var err error
					switch route {
					case "shared":
						_, err = commitAgentFixesWithResult(sctx, types.StepTest, "merge base", "fallback", nil)
					case "ci":
						_, err = (&CIStep{}).commitRepair(sctx, "merge base", nil)
					case "retained":
						_, err = (&CIStep{}).retryProtectedPathRepair(sctx)
					}
					if err != nil {
						t.Fatal(err)
					}
					if parents := commitParents(t, dir, "HEAD"); len(parents) != 2 || parents[0] != head || parents[1] != tip {
						t.Fatalf("parents=%v, want [%s %s]", parents, head, tip)
					}
					if tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); tree != wantTree {
						t.Fatalf("tree=%s, want %s", tree, wantTree)
					}
					if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
						t.Fatal("conformed merge was not recorded")
					}
				})
			}
		}
	}
}

func TestAgentBaseMerge_UnverifiedPRLeavesAgentCommitsUntouched(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, payload, readError, want string
		fresh                          bool
	}{
		{"different PR", `[{"number":43,"url":"https://github.com/test/repo/pull/43","baseRefName":"main"}]`, "", "does not match recorded PR", false},
		{"missing recorded PR", `[]`, "", "does not match recorded PR", false},
		{"missing base", `[{"number":42,"url":"https://github.com/test/repo/pull/42"}]`, "", "base branch is empty", false},
		{"unreadable discovery", `[]`, "provider unavailable", "provider unavailable", false},
		{"fresh discovery error", `[]`, "provider unavailable", "provider unavailable", true},
		{"fresh missing base", `[{"number":42,"url":"https://github.com/test/repo/pull/42"}]`, "", "base branch is empty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, dir, head := agentCommitContext(t, nil)
			advanceMain(t, dir, "upstream.txt", "landed on main\n")
			gitCmd(t, dir, "merge", "--no-ff", "-m", "agent merge", "main")
			merged := gitCmd(t, dir, "rev-parse", "HEAD")
			if !tc.fresh {
				sctx.Run.PRURL = strptr("https://github.com/test/repo/pull/42")
			}
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "pr-discovery", "FAKE_CLI_PR_LIST_JSON": tc.payload, "FAKE_CLI_PR_LIST_ERR": tc.readError})
			_, err := commitAgentFixesWithResult(sctx, types.StepReview, "merge base", "fallback", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %s", err, tc.want)
			}
			if gitCmd(t, dir, "rev-parse", "HEAD") != merged || sctx.Run.HeadSHA != head {
				t.Fatal("refused merge changed the agent's commits or recorded head")
			}
		})
	}
}
