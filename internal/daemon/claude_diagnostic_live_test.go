package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"github.com/agynio/agynd-cli/internal/platform"
	claude "github.com/agynio/claude-sdk-go"
)

func TestClaudeDiagnosticNative401(t *testing.T) {
	if os.Getenv("AGYN_CLAUDE_DIAGNOSTIC_TEST") != "true" {
		t.Skip("requires an explicitly selected native CLI; uses only a loopback error server")
	}
	binary := os.Getenv("AGYN_CLAUDE_DIAGNOSTIC_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("native CLI path must be explicit and absolute")
	}
	root := t.TempDir()
	home, work := filepath.Join(root, "home"), filepath.Join(root, "workspace")
	for _, directory := range []string{home, work} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"HOME": home, "CLAUDE_CONFIG_DIR": "",
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"XDG_DATA_HOME":     filepath.Join(home, ".local", "share"),
		"ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
		"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "", "CLAUDE_CODE_USE_FOUNDRY": "",
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "http_proxy": "", "https_proxy": "", "all_proxy": "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_AUTOUPDATER":                      "1",
	} {
		t.Setenv(key, value)
	}
	// Unset selects the default HOME layout; an explicitly empty path is invalid
	// when the independently proposed session-persistence adapter is present.
	if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeState(); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "claude-no-tools")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec \"$AGYN_CLAUDE_DIAGNOSTIC_BINARY\" --tools \"\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		parsed := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request) == nil
		if r.URL.Path == "/v1/messages" {
			requests.Add(1)
			t.Logf("local message request: parsed=%t streaming=%t", parsed, request.Stream)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		w.Header().Set("request-id", "diagnostic-fixture")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"sensitive diagnostic fixture body"}}`)
	}))
	defer server.Close()
	defer func() { t.Logf("local_http_requests=%d", requests.Load()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		t.Fatalf("native CLI version failed: %v", err)
	}
	t.Logf("native=%s uid=%d", strings.TrimSpace(string(version)), os.Geteuid())
	client, err := claude.Start(ctx, claude.Options{
		BinaryPath: shim, WorkDir: work, Model: "sonnet", MaxTurns: 1, Stderr: io.Discard,
		Env: []string{"ANTHROPIC_BASE_URL=" + server.URL, "ANTHROPIC_API_KEY=local-diagnostic-fixture"},
	})
	if err != nil {
		t.Fatalf("native CLI initialization failed: %v", err)
	}
	defer client.Close()
	t.Log("native CLI initialized; submitting the diagnostic turn")
	threads := &fakeClaudeThreadsClient{}
	daemon := &Daemon{sdk: SDKClaude, claude: &nativeDiagnosticClient{Client: client, t: t}, claudeReady: true, threads: platform.NewThreads(threads),
		agent: &agentsv1.Agent{FinalMessage: agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD}}
	err = daemon.handleClaudeMessage(ctx, platform.Message{ID: "diagnostic-message", ThreadID: "diagnostic-thread", Body: "Return OK."})
	if !errors.Is(err, errClaudeTurnFailed) || !isTerminalAgentProcessingError(err) {
		t.Fatalf("native failure was not terminal: %v", err)
	}
	if !strings.Contains(err.Error(), "api_status=401") {
		t.Fatalf("native HTTP status lost: %v", err)
	}
	if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "local-diagnostic-fixture") {
		t.Fatal("native error content escaped")
	}
	if requests.Load() == 0 {
		t.Fatal("native CLI did not reach the local error server")
	}
	if len(threads.sendRequests) != 0 || len(threads.ackRequests) != 0 {
		t.Fatal("failed native turn published or acknowledged")
	}
	t.Logf("native=%s local_http_requests=%d diagnostic=%v", strings.TrimSpace(string(version)), requests.Load(), err)
}

type nativeDiagnosticClient struct {
	*claude.Client
	t *testing.T
}

func (c *nativeDiagnosticClient) Turn(ctx context.Context, params claude.TurnParams, handler claude.EventHandler) (*claude.TurnResult, error) {
	return c.Client.Turn(ctx, params, func(event claude.Event) {
		// Only lifecycle names are diagnostic output, never event payloads.
		subtype := "unknown"
		if system, ok := event.(claude.SystemMessage); ok {
			switch system.Subtype {
			case "init", "api_retry", "status":
				subtype = system.Subtype
			}
		}
		c.t.Logf("native event: type=%s subtype=%s", event.EventType(), subtype)
		if handler != nil {
			handler(event)
		}
	})
}
