//go:build initimage

// Package initimage checks a built agynd init image the way a restricted
// workload uses it, with Docker standing in for the kubelet:
//
//  1. The image's entrypoint runs as UID/GID 10001, read-only, with no
//     capabilities, into an empty world-writable /agyn (an emptyDir), twice.
//  2. A glibc workspace image with no Node of its own runs the delivered agynd
//     as UID 10001. agynd runs kind-a2a's execution-reporting gate as a required
//     environment init script, which writes /run/agyn-execution/gate.json.
//  3. The receiver runs on a TTY the way Agyn's TerminalGateway runs it, with
//     kind-a2a's receiverCommand and the workload proxy environment every task
//     Pod carries: /agyn/bin/node --no-warnings -e "$(cat
//     agyn-execution-receiver.cjs)". It must report ready, accept the binding,
//     and print the runtime's configured.json, with no other terminal output.
//  4. agynd then starts the agent CLI (test/fakeclaude) only after the MCP
//     sidecar answers, with both MCP servers declared in ~/.claude.json, and
//     acknowledges without a turn the inbox item the binding retired.
//
// The gate and receiver are fetched from smartphonekey/kind-a2a at a pinned
// revision and checked against their SHA-256; the reporting runtime itself is
// the testdata double. INIT_IMAGE names the image under test.
//
//	INIT_IMAGE=<ref> go test -tags initimage -v -count=1 ./test/initimage/
package initimage

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	agentID       = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a01"
	instanceID    = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a02"
	workloadID    = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a03"
	environmentID = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a04"
	executionID   = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a05"
	requestID     = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a06"
	retiredID     = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a07"
	threadID      = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a08"
	senderID      = "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a0a"

	workloadUID = 10001

	// The kind-a2a revision deployed with this contract and its script hashes.
	kindA2ARevision = "90209d5638f118f2db705cbabc3259ecf5b39546"
	receiverSHA256  = "713d79b272fbef421203ef8ea11f35fccfed834b62604a2d441fbe2b8616b9b4"
	gateSHA256      = "6d9bd56441db32f0c915e7b70338e3df21bae2ffbbf3305dcd42415cf8dcc71d"

	// A glibc workspace with no Node: /agyn/bin/node must be self-sufficient.
	workspaceImage = "docker.io/library/debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251"
	// The QA workspace image's base (smartphonekey/infra Dockerfile.qa-tools).
	qaBaseImage = "docker.io/library/node:24-bookworm-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6"
)

var restricted = []string{
	"--user", fmt.Sprintf("%d:%d", workloadUID, workloadUID),
	"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
}

func TestInitImageDeliversAReportingCapableAgynd(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("INIT_IMAGE"))
	if image == "" {
		t.Fatal("INIT_IMAGE must name the init image under test")
	}
	receiver := fetchKindA2A(t, "scripts/agyn-execution-receiver.cjs", receiverSHA256)
	gate := fetchKindA2A(t, "scripts/agyn-execution-gate.cjs", gateSHA256)
	work := workloadScratch(t)
	agyn := worldWritableDir(t, work, "agyn")
	workspace := worldWritableDir(t, work, "workspace")

	// An init container can run again in the same Pod; the copy must hold.
	for range 2 {
		docker(t, append(append([]string{"run", "--rm", "--network", "none"}, restricted...),
			"-v", agyn+":/agyn", image)...)
	}
	assertDelivered(t, agyn)

	// What the agent-runtime init container adds beside agynd, as the same user.
	runtimeSource := t.TempDir()
	buildFakeClaude(t, filepath.Join(runtimeSource, "claude"))
	writeFile(t, filepath.Join(runtimeSource, "config.json"), `{"sdk":"claude","bin":"bin/claude"}`, 0o644)
	docker(t, append(append([]string{"run", "--rm", "--network", "none"}, restricted...),
		"-v", agyn+":/agyn", "-v", runtimeSource+":/runtime:ro", "--entrypoint", "/bin/sh", workspaceImage,
		"-c", "cp /runtime/claude /agyn/bin/claude && chmod 0755 /agyn/bin/claude && cp /runtime/config.json /agyn/config.json")...)

	runtime, err := os.ReadFile(filepath.Join("testdata", "runtime.mjs"))
	if err != nil {
		t.Fatalf("read runtime double: %v", err)
	}
	runtimeSum := sha256.Sum256(runtime)
	runtimeSHA256 := hex.EncodeToString(runtimeSum[:])

	// kind-a2a registers the gate as an environment init script in exactly this form.
	gateway := startGateway(t, fmt.Sprintf("/agyn/bin/node <<'AGYN_EXECUTION_GATE'\n%s\nAGYN_EXECUTION_GATE\n", gate))
	mcpPort, mcpInitializes := startMCPServer(t)

	name := "agynd-init-test-" + randomHex(t, 6)
	args := []string{"run", "-d", "--name", name, "--network", "host"}
	args = append(args, restricted...)
	args = append(args,
		// The gate creates /run/agyn-execution itself, so /run must be writable
		// by the workload user; a QA workspace image has to provide that.
		"--tmpfs", fmt.Sprintf("/run:rw,nosuid,nodev,mode=0755,uid=%d,gid=%d", workloadUID, workloadUID),
		"--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777",
		"-v", agyn+":/agyn", "-v", workspace+":/workspace", "-w", "/workspace",
		"-e", "HOME=/workspace",
		"-e", "AGENT_ID="+agentID,
		"-e", "AGENT_INSTANCE_ID="+instanceID,
		"-e", "WORKLOAD_ID="+workloadID,
		"-e", "ENVIRONMENT_ID="+environmentID,
		"-e", "THREAD_ID="+threadID,
		"-e", "GATEWAY_ADDRESS="+gateway.address,
		"-e", "TRACING_ADDRESS=127.0.0.1:1",
		"-e", "LLM_MODE=native",
		"-e", fmt.Sprintf("AGENT_MCP_SERVERS=qa_browser:%d", mcpPort),
		"-e", "WORKSPACE_DIR=/workspace",
		"-e", "AGYN_INIT_SCRIPTS_REQUIRED=true",
		"-e", "AGYN_INBOX_JOURNAL_DIR=/workspace/.agyn/inbox-journal",
		"-e", "AGYN_INBOX_CONTROL_FILE=/run/agyn-execution/inbox-control.json",
		"-e", "A2A_REPORTING_RUNTIME_SHA256="+runtimeSHA256,
		"-e", "FAKE_CLAUDE_RECORD=/workspace/claude-start.json",
		workspaceImage, "/agyn/bin/agynd")
	docker(t, args...)
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Logf("main container log:\n%s", logs)
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	payload, err := json.Marshal(map[string]any{
		"executionId": executionID, "instanceId": instanceID, "workloadId": workloadID, "threadId": threadID,
		"requestId": requestID, "retiredRequestIds": []string{retiredID}, "profileId": "claude-gated-v1",
		"bundle":    gzipBase64(t, runtime),
		"reporting": map[string]any{"url": "https://a2a.example.invalid/reporting", "token": strings.Repeat("A", 43), "allowInsecureLocal": false},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ready, configured := runReceiver(t, name, receiver, payload)
	if want := map[string]any{"ready": true, "instanceId": instanceID, "workloadId": workloadID, "runtimeSha256": runtimeSHA256}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("receiver readiness = %#v, want %#v", ready, want)
	}
	if want := map[string]any{"executionId": executionID, "instanceId": instanceID, "reportingConfigured": true}; !reflect.DeepEqual(configured, want) {
		t.Fatalf("receiver acknowledgement = %#v, want %#v", configured, want)
	}

	select {
	case <-gateway.subscribed:
	case <-time.After(90 * time.Second):
		t.Fatal("agynd did not reach its notification subscription after the gate")
	}
	// The inbox guard reads the control the receiver wrote: the retired request
	// is acknowledged and journaled without running the agent.
	select {
	case <-gateway.acked:
	case <-time.After(60 * time.Second):
		t.Fatal("agynd did not acknowledge the retired inbox item")
	}
	journal := docker(t, "exec", name, "/bin/sh", "-c", "cat /workspace/.agyn/inbox-journal/"+instanceID+"/*")
	if !strings.Contains(journal, `"message_id":"`+retiredID+`"`) || !strings.Contains(journal, `"state":"ack_only"`) {
		t.Fatalf("inbox journal does not record the retired item as ack-only: %s", journal)
	}

	assertExecutionFiles(t, name)
	assertClaudeStart(t, name, mcpPort)
	if mcpInitializes() < 2 {
		t.Fatalf("expected agynd's readiness probe and the CLI's initialize, got %d", mcpInitializes())
	}
	logs, err := exec.Command("docker", "logs", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	if !strings.Contains(string(logs), `shell server started on socket "agyn"`) {
		t.Fatalf("the delivered tmux did not start as UID %d", workloadUID)
	}
}

// The QA workspace image runs as UID 10001 on a Debian base whose /run is
// root-owned. The gate cannot create /run/agyn-execution there; this records
// what that base provides without failing the image under test.
func TestQAWorkspaceBaseRunDirectory(t *testing.T) {
	docker(t, "pull", "-q", qaBaseImage)
	// The main container keeps the image's writable root; only the user is restricted.
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--user", fmt.Sprintf("%d:%d", workloadUID, workloadUID),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint", "/bin/sh", qaBaseImage,
		"-c", "mkdir /run/agyn-execution && echo writable")
	output, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(string(output), "writable") {
		t.Logf("%s: /run is writable by UID %d", qaBaseImage, workloadUID)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	fmt.Printf("::warning title=QA workspace /run::%s does not let UID %d create /run/agyn-execution (%s); the reporting gate fails unless the workspace image makes /run writable for it\n",
		qaBaseImage, workloadUID, lines[len(lines)-1])
}

func assertDelivered(t *testing.T, agyn string) {
	t.Helper()
	for path, mode := range map[string]fs.FileMode{
		"bin/agynd": 0o755, "bin/agynd-trace-hook": 0o755, "bin/tmux": 0o755, "bin/node": 0o755, "bin/node.bin": 0o755,
		"lib/libstdc++.so.6": 0, "lib/libgcc_s.so.1": 0, "tmux.conf": 0o644, "terminfo/x/xterm-256color": 0, "terminfo/t/tmux-256color": 0,
	} {
		info, err := os.Stat(filepath.Join(agyn, path))
		if err != nil {
			t.Fatalf("/agyn/%s not delivered: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("/agyn/%s is not a regular file", path)
		}
		if mode != 0 && info.Mode().Perm() != mode {
			t.Fatalf("/agyn/%s mode = %v, want %v", path, info.Mode().Perm(), mode)
		}
		if uid := info.Sys().(*syscall.Stat_t).Uid; uid != workloadUID {
			t.Fatalf("/agyn/%s owned by UID %d, want %d", path, uid, workloadUID)
		}
	}
	info, err := os.Stat(filepath.Join(agyn, "run"))
	if err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("/agyn/run must be mode 0777: %v %v", info, err)
	}
}

// assertExecutionFiles checks what the receiver and gate left, from inside the
// main container with the delivered Node.
func assertExecutionFiles(t *testing.T, name string) {
	t.Helper()
	script := `const fs = require("node:fs"); const d = "/run/agyn-execution"; const out = {};
for (const f of fs.readdirSync(d)) { const s = fs.statSync(d + "/" + f); out[f] = { mode: (s.mode & 0o777).toString(8), uid: s.uid }; }
out.control = JSON.parse(fs.readFileSync(d + "/inbox-control.json", "utf8"));
out.dir = (fs.statSync(d).mode & 0o777).toString(8);
console.log(JSON.stringify(out));`
	output := docker(t, "exec", name, "/agyn/bin/node", "-e", script)
	var state map[string]any
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		t.Fatalf("parse execution state: %v\n%s", err, output)
	}
	if state["dir"] != "700" {
		t.Fatalf("/run/agyn-execution mode = %v, want 700", state["dir"])
	}
	for file, mode := range map[string]string{"gate.json": "600", "runtime.mjs": "500", "binding.json": "600",
		"expected.json": "600", "inbox-control.json": "600", "received": "600", "configured.json": "600"} {
		entry, _ := state[file].(map[string]any)
		if entry == nil || entry["mode"] != mode || entry["uid"] != float64(workloadUID) {
			t.Fatalf("%s = %#v, want mode %s owned by %d", file, entry, mode, workloadUID)
		}
	}
	want := map[string]any{"version": float64(1), "instance_id": instanceID, "allowed_message_id": requestID, "ack_only_message_ids": []any{retiredID}}
	if !reflect.DeepEqual(state["control"], want) {
		t.Fatalf("inbox control = %#v, want %#v", state["control"], want)
	}
}

// assertClaudeStart reads the CLI's start record, private to the workload user.
func assertClaudeStart(t *testing.T, name string, mcpPort int) {
	t.Helper()
	data := []byte(docker(t, "exec", name, "cat", "/workspace/claude-start.json"))
	var record struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
		Answered   map[string]bool           `json:"answered"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("parse CLI start record: %v\n%s", err, data)
	}
	if want := map[string]any{"type": "http", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", mcpPort)}; !reflect.DeepEqual(record.MCPServers["qa_browser"], want) {
		t.Fatalf("qa_browser declaration = %#v, want %#v", record.MCPServers["qa_browser"], want)
	}
	if !record.Answered["qa_browser"] {
		t.Fatalf("the CLI started before its MCP sidecar answered: %s", data)
	}
	reporting := record.MCPServers["execution_reporting"]
	if reporting["type"] != "stdio" || reporting["command"] != "/agyn/bin/node" {
		t.Fatalf("execution_reporting declaration = %#v", reporting)
	}
}

// runReceiver starts the receiver on a TTY, as TerminalGateway does, and
// delivers the binding only after the receiver reports ready.
func runReceiver(t *testing.T, container, receiver string, payload []byte) (map[string]any, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	receiverFile := filepath.Join(dir, "agyn-execution-receiver.cjs")
	writeFile(t, receiverFile, receiver, 0o600)
	wrapper := filepath.Join(dir, "terminal.sh")
	// The PTY merges stderr into the protocol: under NODE_USE_ENV_PROXY this
	// Node prints an EnvHttpProxyAgent warning after the ready line, which the
	// strict reader below rejects, unless the command passes --no-warnings.
	writeFile(t, wrapper, "#!/bin/sh\nexec docker exec -it"+
		" -e NODE_USE_ENV_PROXY=1 -e HTTP_PROXY=http://127.0.0.1:18080 -e HTTPS_PROXY=http://127.0.0.1:18080"+
		" \"$CONTAINER\" /agyn/bin/node --no-warnings -e \"$(cat \"$RECEIVER_FILE\")\"\n", 0o700)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// util-linux script gives the docker client the terminal docker exec -t needs.
	cmd := exec.CommandContext(ctx, "script", "-qfec", wrapper, "/dev/null")
	cmd.Env = append(os.Environ(), "CONTAINER="+container, "RECEIVER_FILE="+receiverFile)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start terminal: %v", err)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if strings.TrimSpace(line) != "" {
				lines <- strings.TrimSpace(line)
			}
			if err != nil {
				return
			}
		}
	}()
	next := func(stage string) map[string]any {
		select {
		case line, ok := <-lines:
			if !ok {
				_ = cmd.Wait()
				t.Fatalf("terminal closed before %s; stderr: %s", stage, stderr.String())
			}
			var value map[string]any
			// Strict, like kind-a2a's installer: any other output is a protocol error.
			if err := json.Unmarshal([]byte(line), &value); err != nil {
				t.Fatalf("%s line is not JSON: %q", stage, line)
			}
			return value
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", stage)
		}
		return nil
	}
	ready := next("readiness")
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		t.Fatalf("deliver binding: %v", err)
	}
	configured := next("acknowledgement")
	for range lines {
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("receiver did not exit cleanly: %v; stderr: %s", err, stderr.String())
	}
	_ = stdin.Close()
	return ready, configured
}

type gatewayStub struct {
	address    string
	subscribed chan struct{}
	once       sync.Once
	gate       string

	mu      sync.Mutex
	acked   chan struct{}
	ackOnce sync.Once
	isAcked bool
}

type agentsStub struct {
	gatewayv1.UnimplementedAgentsGatewayServer
	*gatewayStub
}

type threadsStub struct {
	gatewayv1.UnimplementedThreadsGatewayServer
	*gatewayStub
}

type notificationsStub struct {
	gatewayv1.UnimplementedNotificationsGatewayServer
	*gatewayStub
}

type runnersStub struct {
	gatewayv1.UnimplementedRunnersGatewayServer
	*gatewayStub
}

// startGateway serves the calls agynd makes before it subscribes, on host
// loopback, which the host-network main container shares.
func startGateway(t *testing.T, gateScript string) *gatewayStub {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway: %v", err)
	}
	stub := &gatewayStub{address: listener.Addr().String(), subscribed: make(chan struct{}), gate: gateScript, acked: make(chan struct{})}
	server := grpc.NewServer()
	gatewayv1.RegisterAgentsGatewayServer(server, agentsStub{gatewayStub: stub})
	gatewayv1.RegisterThreadsGatewayServer(server, threadsStub{gatewayStub: stub})
	gatewayv1.RegisterNotificationsGatewayServer(server, notificationsStub{gatewayStub: stub})
	gatewayv1.RegisterRunnersGatewayServer(server, runnersStub{gatewayStub: stub})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return stub
}

func (s agentsStub) GetAgent(context.Context, *agentsv1.GetAgentRequest) (*agentsv1.GetAgentResponse, error) {
	return &agentsv1.GetAgentResponse{Agent: &agentsv1.Agent{
		Meta: &agentsv1.EntityMeta{Id: agentID}, Name: "init-image-test", Model: "claude-test",
	}}, nil
}

func (s agentsStub) ListSkills(context.Context, *agentsv1.ListSkillsRequest) (*agentsv1.ListSkillsResponse, error) {
	return &agentsv1.ListSkillsResponse{}, nil
}

func (s agentsStub) ListInitScripts(_ context.Context, req *agentsv1.ListInitScriptsRequest) (*agentsv1.ListInitScriptsResponse, error) {
	if req.GetEnvironmentId() != environmentID {
		return &agentsv1.ListInitScriptsResponse{}, nil
	}
	return &agentsv1.ListInitScriptsResponse{InitScripts: []*agentsv1.InitScript{{
		Meta:        &agentsv1.EntityMeta{Id: "6f1d8a52-9d4e-4f7c-a8b1-1f0e6e2b7a09", CreatedAt: timestamppb.Now()},
		Script:      s.gate,
		Description: "Trusted-local execution reporting gate; fail closed before the agent starts",
	}}}, nil
}

// GetUnackedInboxItems offers the request the binding retired until it is acked.
func (s agentsStub) GetUnackedInboxItems(_ context.Context, req *agentsv1.GetUnackedInboxItemsRequest) (*agentsv1.GetUnackedInboxItemsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetAgentInstanceId() != instanceID || s.isAcked {
		return &agentsv1.GetUnackedInboxItemsResponse{}, nil
	}
	return &agentsv1.GetUnackedInboxItemsResponse{Items: []*agentsv1.InboxItem{{
		Id: retiredID, AgentInstanceId: instanceID, SourceKind: agentsv1.InboxItemSourceKind_INBOX_ITEM_SOURCE_KIND_DIRECT,
		SenderId: senderID, Body: "A request an earlier workload may have started", AcceptedAt: timestamppb.Now(),
	}}}, nil
}

func (s agentsStub) AckInboxItems(_ context.Context, req *agentsv1.AckInboxItemsRequest) (*agentsv1.AckInboxItemsResponse, error) {
	if req.GetAgentInstanceId() != instanceID || len(req.GetItemIds()) != 1 || req.GetItemIds()[0] != retiredID {
		return nil, fmt.Errorf("unexpected acknowledgement %v", req.GetItemIds())
	}
	s.mu.Lock()
	s.isAcked = true
	s.mu.Unlock()
	s.ackOnce.Do(func() { close(s.acked) })
	return &agentsv1.AckInboxItemsResponse{}, nil
}

func (s threadsStub) GetUnackedMessages(context.Context, *threadsv1.GetUnackedMessagesRequest) (*threadsv1.GetUnackedMessagesResponse, error) {
	return &threadsv1.GetUnackedMessagesResponse{}, nil
}

func (s notificationsStub) Subscribe(_ *notificationsv1.SubscribeRequest, stream grpc.ServerStreamingServer[notificationsv1.SubscribeResponse]) error {
	s.once.Do(func() { close(s.subscribed) })
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s runnersStub) TouchWorkload(context.Context, *runnersv1.TouchWorkloadRequest) (*runnersv1.TouchWorkloadResponse, error) {
	return &runnersv1.TouchWorkloadResponse{}, nil
}

// startMCPServer is the MCP sidecar: it answers initialize on host loopback,
// and only after a delay, as a sidecar starting beside the main container does.
func startMCPServer(t *testing.T) (int, func() int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen MCP: %v", err)
	}
	var mu sync.Mutex
	initializes := 0
	open := time.Now().Add(5 * time.Second)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(open) {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.URL.Path != "/mcp" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "initialize" {
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}
		mu.Lock()
		initializes++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"qa_browser","version":"0"}}}`, request.ID)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port, func() int {
		mu.Lock()
		defer mu.Unlock()
		return initializes
	}
}

func fetchKindA2A(t *testing.T, path, wantSHA256 string) string {
	t.Helper()
	url := fmt.Sprintf("https://raw.githubusercontent.com/smartphonekey/kind-a2a/%s/%s", kindA2ARevision, path)
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("fetch %s: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fetch %s: %s", url, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
		t.Fatalf("%s sha256 = %s, want %s", path, got, wantSHA256)
	}
	return string(data)
}

func buildFakeClaude(t *testing.T, destination string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", destination, "./test/fakeclaude")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake claude: %v\n%s", err, output)
	}
	if err := os.Chmod(destination, 0o755); err != nil {
		t.Fatal(err)
	}
}

// workloadScratch is a host directory whose contents the workload user owns;
// they are removed as that user before the directory itself.
func workloadScratch(t *testing.T) string {
	t.Helper()
	work, err := os.MkdirTemp("", "agynd-initimage-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", append(append([]string{"run", "--rm", "--network", "none"}, restricted...),
			"-v", work+":/work", "--entrypoint", "/bin/sh", workspaceImage,
			"-c", "for d in /work/*; do find \"$d\" -mindepth 1 -delete; done")...).Run()
		_ = os.RemoveAll(work)
	})
	return work
}

// worldWritableDir is an emptyDir: mode 0777 whatever the umask.
func worldWritableDir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func gzipBase64(t *testing.T, data []byte) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s%s", args[0], err, stdout.String(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
