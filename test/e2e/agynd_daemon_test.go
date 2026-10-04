//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	gatewayv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/gateway/v1"
	notificationsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/notifications/v1"
	runnersv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/runners/v1"
	threadsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/threads/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	agyndE2EAgentID    = "550e8400-e29b-41d4-a716-446655440000"
	agyndE2EThreadID   = "550e8400-e29b-41d4-a716-446655440001"
	agyndE2EInstanceID = "550e8400-e29b-41d4-a716-446655440003"
	agyndE2EWorkloadID = "550e8400-e29b-41d4-a716-446655440002"
)

func TestAgyndBinaryInitializesWithStubGateway(t *testing.T) {
	binary := buildAgynd(t)
	agentBinary := buildFakeAgnAgent(t)
	server := startAgyndGatewayStub(t)
	workDir := t.TempDir()
	installAgentRuntimeConfig(t, agentBinary)

	cmd := exec.Command(binary)
	cmd.Dir = workDir
	cmd.Env = append(cleanAgyndEnv(),
		"AGENT_ID="+agyndE2EAgentID,
		"AGENT_INSTANCE_ID="+agyndE2EInstanceID,
		"WORKLOAD_ID="+agyndE2EWorkloadID,
		"GATEWAY_ADDRESS="+server.address,
		"TRACING_ADDRESS=127.0.0.1:1",
		"LLM_BASE_URL=https://testllm.dev/v1/org/agynio/suite/agn",
		"LLM_API_TOKEN=test-token",
		"AGN_TOKEN_COUNTING_ADDRESS=127.0.0.1:1",
		"HOME="+t.TempDir(),
		"WORKSPACE_DIR="+workDir,
	)

	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agynd: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case <-server.runStarted:
	case err := <-waitCh:
		t.Fatalf("agynd exited before daemon.Run/subscriber startup: %v\noutput:\n%s", err, output.String())
	case <-time.After(10 * time.Second):
		terminateProcess(cmd.Process)
		<-waitCh
		t.Fatalf("agynd did not reach daemon.Run/subscriber startup; calls: %s\noutput:\n%s", server.callsSummary(), output.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		terminateProcess(cmd.Process)
		<-waitCh
		t.Fatalf("interrupt agynd: %v\noutput:\n%s", err, output.String())
	}
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		terminateProcess(cmd.Process)
		<-waitCh
		t.Fatalf("agynd did not stop after interrupt; output:\n%s", output.String())
	}

	server.assertInitialized(t)
	server.assertRunStarted(t)
}

func terminateProcess(process *os.Process) {
	if process == nil {
		return
	}
	_ = process.Signal(os.Interrupt)
	time.Sleep(100 * time.Millisecond)
	_ = process.Signal(syscall.SIGKILL)
}

func cleanAgyndEnv() []string {
	blocked := map[string]struct{}{
		"AGENT_MCP_SERVERS": {},
		"MCP_PORT":          {},
	}
	env := os.Environ()
	cleaned := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, skip := blocked[key]; skip {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

func buildAgynd(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), ".tmp-e2e", "agynd-"+strings.ReplaceAll(t.Name(), "/", "-"))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create agynd build dir: %v", err)
	}
	path := filepath.Join(dir, "agynd")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, "./cmd/agynd")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build agynd: %v\n%s", err, output)
	}
	return path
}

func installAgentRuntimeConfig(t *testing.T, agentBinary string) {
	t.Helper()
	installAgentRuntime(t, "agn", agentBinary)
}

// installAgentRuntime does what an agent runtime image does: it places the CLI
// under the shared volume and names its SDK in /agyn/config.json.
func installAgentRuntime(t *testing.T, sdk, agentBinary string) {
	t.Helper()
	runner := newPrivilegedRunner(t)
	runner.run(t, "mkdir", "-p", "/agyn/bin")

	backupPath := filepath.Join(t.TempDir(), "config.json.backup")
	hadExisting := fileExists("/agyn/config.json")
	if hadExisting {
		runner.run(t, "cp", "/agyn/config.json", backupPath)
	}
	t.Cleanup(func() {
		if hadExisting {
			runner.run(t, "cp", backupPath, "/agyn/config.json")
			return
		}
		runner.run(t, "rm", "-f", "/agyn/config.json")
	})

	// bin is relative to the volume and resolved against it, so the agent has
	// to live under the volume rather than wherever the test built it. This is
	// what a runtime image does: it states what it carries, not where the
	// platform mounted it.
	installedName := filepath.Base(agentBinary)
	installedPath := filepath.Join("/agyn/bin", installedName)
	runner.run(t, "cp", agentBinary, installedPath)
	runner.run(t, "chmod", "0755", installedPath)
	t.Cleanup(func() { runner.run(t, "rm", "-f", installedPath) })

	configPath := filepath.Join(t.TempDir(), "config.json")
	payload := fmt.Sprintf(`{"sdk":%q,"bin":%q}`, sdk, filepath.Join("bin", installedName))
	if err := os.WriteFile(configPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	runner.run(t, "cp", configPath, "/agyn/config.json")
	runner.run(t, "chmod", "0644", "/agyn/config.json")
}

type privilegedRunner struct {
	sudo string
}

func newPrivilegedRunner(t *testing.T) privilegedRunner {
	t.Helper()
	if sudo, err := exec.LookPath("sudo"); err == nil {
		return privilegedRunner{sudo: sudo}
	}
	if os.Geteuid() == 0 {
		return privilegedRunner{}
	}
	t.Fatal("sudo is required to install /agyn/config.json for agynd e2e")
	return privilegedRunner{}
}

func (r privilegedRunner) run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmdArgs := args
	if r.sudo != "" {
		cmdArgs = append([]string{name}, args...)
		name = r.sudo
	}
	if output, err := exec.Command(name, cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("run %s %s: %v\n%s", name, strings.Join(cmdArgs, " "), err, output)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func buildFakeAgnAgent(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agn")
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(fakeAgnAgentSource), 0o600); err != nil {
		t.Fatalf("write fake agn source: %v", err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fake agn agent: %v\n%s", err, output)
	}
	return path
}

const fakeAgnAgentSource = "package main\n\nimport (\n\t\"bufio\"\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n)\n\ntype request struct {\n\tJSONRPC string          `json:\"jsonrpc\"`\n\tID      json.RawMessage `json:\"id\"`\n\tMethod  string          `json:\"method\"`\n\tParams  json.RawMessage `json:\"params\"`\n}\n\ntype response struct {\n\tJSONRPC string          `json:\"jsonrpc\"`\n\tID      json.RawMessage `json:\"id\"`\n\tResult  json.RawMessage `json:\"result\"`\n}\n\nfunc main() {\n\tif len(os.Args) < 2 || os.Args[1] != \"serve\" {\n\t\tos.Exit(2)\n\t}\n\tscanner := bufio.NewScanner(os.Stdin)\n\tfor scanner.Scan() {\n\t\tvar req request\n\t\tif err := json.Unmarshal(scanner.Bytes(), &req); err != nil {\n\t\t\tcontinue\n\t\t}\n\t\tresult := json.RawMessage(`{\"thread_id\":\"thread-from-fake-agent\",\"response\":\"ok\"}`)\n\t\tif req.Method == \"thread/list\" {\n\t\t\tresult = json.RawMessage(`{\"data\":[],\"next_cursor\":null}`)\n\t\t}\n\t\tpayload, _ := json.Marshal(response{JSONRPC: \"2.0\", ID: req.ID, Result: result})\n\t\tfmt.Println(string(payload))\n\t}\n}\n"

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

type agyndGatewayStub struct {
	address    string
	server     *grpc.Server
	runStarted chan struct{}
	runOnce    sync.Once

	mu                   sync.Mutex
	getAgentCalls        int
	listSkillsCalls      int
	listMCPsCalls        int
	listInitScriptsCalls int
}

type agyndAgentsGatewayStub struct {
	gatewayv1.UnimplementedAgentsGatewayServer
	*agyndGatewayStub
}

type agyndThreadsGatewayStub struct {
	gatewayv1.UnimplementedThreadsGatewayServer
	*agyndGatewayStub
}

type agyndNotificationsGatewayStub struct {
	gatewayv1.UnimplementedNotificationsGatewayServer
	*agyndGatewayStub
}

type agyndRunnersGatewayStub struct {
	gatewayv1.UnimplementedRunnersGatewayServer
	*agyndGatewayStub
}

func startAgyndGatewayStub(t *testing.T) *agyndGatewayStub {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway stub: %v", err)
	}
	stub := &agyndGatewayStub{address: listener.Addr().String(), runStarted: make(chan struct{})}
	stub.server = grpc.NewServer()
	gatewayv1.RegisterAgentsGatewayServer(stub.server, agyndAgentsGatewayStub{agyndGatewayStub: stub})
	gatewayv1.RegisterThreadsGatewayServer(stub.server, agyndThreadsGatewayStub{agyndGatewayStub: stub})
	gatewayv1.RegisterNotificationsGatewayServer(stub.server, agyndNotificationsGatewayStub{agyndGatewayStub: stub})
	gatewayv1.RegisterRunnersGatewayServer(stub.server, agyndRunnersGatewayStub{agyndGatewayStub: stub})

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- stub.server.Serve(listener)
	}()
	t.Cleanup(func() {
		stub.server.Stop()
		_ = listener.Close()
		select {
		case err := <-serveErr:
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				t.Errorf("gateway stub stopped with error: %v", err)
			}
		default:
		}
	})
	return stub
}

func (s agyndAgentsGatewayStub) GetAgent(_ context.Context, req *agentsv1.GetAgentRequest) (*agentsv1.GetAgentResponse, error) {
	if req.GetId() != agyndE2EAgentID {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected agent id %q", req.GetId())
	}
	s.mu.Lock()
	s.getAgentCalls++
	s.mu.Unlock()
	return &agentsv1.GetAgentResponse{Agent: &agentsv1.Agent{
		Meta:  &agentsv1.EntityMeta{Id: agyndE2EAgentID},
		Name:  "agynd-e2e-agent",
		Model: "simple-hello",
	}}, nil
}

func (s agyndAgentsGatewayStub) ListSkills(_ context.Context, req *agentsv1.ListSkillsRequest) (*agentsv1.ListSkillsResponse, error) {
	if req.GetAgentId() != agyndE2EAgentID {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected skills agent id %q", req.GetAgentId())
	}
	s.mu.Lock()
	s.listSkillsCalls++
	s.mu.Unlock()
	return &agentsv1.ListSkillsResponse{}, nil
}

func (s agyndAgentsGatewayStub) ListMcps(_ context.Context, req *agentsv1.ListMcpsRequest) (*agentsv1.ListMcpsResponse, error) {
	if req.GetAgentId() != agyndE2EAgentID {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected mcps agent id %q", req.GetAgentId())
	}
	s.mu.Lock()
	s.listMCPsCalls++
	s.mu.Unlock()
	return &agentsv1.ListMcpsResponse{}, nil
}

func (s agyndAgentsGatewayStub) ListInitScripts(_ context.Context, req *agentsv1.ListInitScriptsRequest) (*agentsv1.ListInitScriptsResponse, error) {
	if req.GetAgentId() != agyndE2EAgentID {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected init scripts agent id %q", req.GetAgentId())
	}
	s.mu.Lock()
	s.listInitScriptsCalls++
	s.mu.Unlock()
	return &agentsv1.ListInitScriptsResponse{}, nil
}

func (s *agyndGatewayStub) markRunStarted() {
	s.runOnce.Do(func() { close(s.runStarted) })
}

func (s *agyndGatewayStub) waitRunStarted(timeout time.Duration) error {
	select {
	case <-s.runStarted:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for Subscribe; calls: %s", s.callsSummary())
	}
}

func (s agyndThreadsGatewayStub) GetUnackedMessages(context.Context, *threadsv1.GetUnackedMessagesRequest) (*threadsv1.GetUnackedMessagesResponse, error) {
	return &threadsv1.GetUnackedMessagesResponse{}, nil
}

func (s agyndNotificationsGatewayStub) Subscribe(_ *notificationsv1.SubscribeRequest, stream grpc.ServerStreamingServer[notificationsv1.SubscribeResponse]) error {
	s.markRunStarted()
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s agyndRunnersGatewayStub) TouchWorkload(context.Context, *runnersv1.TouchWorkloadRequest) (*runnersv1.TouchWorkloadResponse, error) {
	return &runnersv1.TouchWorkloadResponse{}, nil
}

func (s *agyndGatewayStub) assertInitialized(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	// ListMcps is deliberately absent: the MCP list comes from the Orchestrator
	// rather than the API (#166), so agynd no longer asks the gateway for it.
	// The stub still serves the call, because nothing says it may not be asked.
	checks := map[string]int{
		"GetAgent":        s.getAgentCalls,
		"ListSkills":      s.listSkillsCalls,
		"ListInitScripts": s.listInitScriptsCalls,
	}
	for name, calls := range checks {
		if calls == 0 {
			t.Fatalf("expected agynd to call %s during initialization; calls: %s", name, formatCalls(checks))
		}
	}
}

func (s *agyndGatewayStub) assertRunStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.runStarted:
	default:
		t.Fatal("expected agynd to enter daemon.Run and subscribe to notifications")
	}
}

func (s *agyndGatewayStub) callsSummary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return formatCalls(map[string]int{
		"GetAgent":        s.getAgentCalls,
		"ListSkills":      s.listSkillsCalls,
		"ListMcps":        s.listMCPsCalls,
		"ListInitScripts": s.listInitScriptsCalls,
	})
}

func formatCalls(calls map[string]int) string {
	return fmt.Sprintf("GetAgent=%d ListSkills=%d ListMcps=%d ListInitScripts=%d", calls["GetAgent"], calls["ListSkills"], calls["ListMcps"], calls["ListInitScripts"])
}
