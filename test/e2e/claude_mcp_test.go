//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// claudeStartRecord is what the fake Claude CLI saw the moment agynd started it.
type claudeStartRecord struct {
	MCPServers map[string]map[string]any `json:"mcpServers"`
	Answered   map[string]bool           `json:"answered"`
}

// Claude Code connects to its MCP servers once, when it starts, and reads
// user-scoped servers only from its .claude.json. agynd must declare the
// platform's servers there and start the CLI only after they answer, without
// dropping a server something else declared in the same file.
func TestAgyndStartsClaudeAfterDeclaredMCPServersAnswer(t *testing.T) {
	binary := buildAgynd(t)
	claudeBinary := buildFakeClaude(t)
	server := startAgyndGatewayStub(t)
	workDir := t.TempDir()
	home := t.TempDir()
	record := filepath.Join(t.TempDir(), "claude-start.json")
	installAgentRuntime(t, "claude", claudeBinary)

	port := reserveLoopbackPort(t)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	// Declared before agynd starts, as an execution-reporting init script or a
	// workspace entrypoint would; agynd owns only the names it is given.
	existing := `{"mcpServers":{"execution_reporting":{"type":"stdio","command":"/agyn/bin/node","args":["runtime.mjs","mcp"]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing user state: %v", err)
	}

	cmd := exec.Command(binary)
	cmd.Dir = workDir
	cmd.Env = append(cleanAgyndEnv(),
		"AGENT_ID="+agyndE2EAgentID,
		"AGENT_INSTANCE_ID="+agyndE2EInstanceID,
		"WORKLOAD_ID="+agyndE2EWorkloadID,
		"GATEWAY_ADDRESS="+server.address,
		"TRACING_ADDRESS=127.0.0.1:1",
		"LLM_MODE=native",
		fmt.Sprintf("AGENT_MCP_SERVERS=late:%d", port),
		"FAKE_CLAUDE_RECORD="+record,
		"HOME="+home,
		"WORKSPACE_DIR="+workDir,
	)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agynd: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-waitCh:
		default:
			terminateProcess(cmd.Process)
			<-waitCh
		}
	})

	// The sidecar comes up after the main container, as it does in a Pod.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(record); err == nil {
		t.Fatalf("Claude started before its MCP server listened; output:\n%s", output.String())
	}
	var initializes atomic.Int32
	startLateMCPServer(t, port, &initializes)

	select {
	case <-server.runStarted:
	case err := <-waitCh:
		t.Fatalf("agynd exited before daemon.Run: %v\noutput:\n%s", err, output.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("agynd did not reach daemon.Run; calls: %s\noutput:\n%s", server.callsSummary(), output.String())
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake Claude did not record its start: %v\noutput:\n%s", err, output.String())
	}
	var started claudeStartRecord
	if err := json.Unmarshal(data, &started); err != nil {
		t.Fatalf("parse start record: %v\n%s", err, data)
	}
	if want := map[string]any{"type": "http", "url": endpoint}; !reflect.DeepEqual(started.MCPServers["late"], want) {
		t.Fatalf("late MCP declaration = %#v, want %#v", started.MCPServers["late"], want)
	}
	if started.MCPServers["execution_reporting"]["type"] != "stdio" {
		t.Fatalf("existing MCP declaration dropped: %#v", started.MCPServers)
	}
	if !started.Answered["late"] {
		t.Fatalf("Claude started before the late MCP server answered: %s\noutput:\n%s", data, output.String())
	}
	if initializes.Load() < 2 {
		t.Fatalf("expected agynd's readiness probe and Claude's own initialize, got %d", initializes.Load())
	}

	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(settings), "mcpServers") {
		t.Fatalf("settings.json still declares MCP servers Claude ignores there:\n%s", settings)
	}
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// startLateMCPServer answers MCP initialize on the reserved port.
func startLateMCPServer(t *testing.T, port int, initializes *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen MCP server: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "initialize" {
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}
		initializes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"late","version":"0"}}}`, request.ID)
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
}

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, "./test/fakeclaude")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake claude: %v\n%s", err, output)
	}
	return path
}
