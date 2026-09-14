package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	claude "github.com/agynio/claude-sdk-go"
	"golang.org/x/net/dns/dnsmessage"
)

// Run only in the operator's network-denied disposable Pod. Its DNS config
// points solely at these loopback servers; no real provider receives a request.
func TestNativeDNSInterception(t *testing.T) {
	if os.Getenv("AGYN_NATIVE_DNS_TEST") != "true" {
		t.Skip("requires isolated native DNS fixture")
	}
	mode := os.Getenv("AGYN_NATIVE_DNS_MODE")
	if mode != "mixed" && mode != "single" && mode != "unavailable" {
		t.Fatal("explicit DNS mode required")
	}
	wantDNS := []string{"127.0.0.2"}
	if mode == "mixed" {
		wantDNS = append(wantDNS, "127.0.0.3")
	}
	resolv, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	var servers []string
	for _, line := range strings.Split(string(resolv), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	if !reflect.DeepEqual(servers, wantDNS) {
		t.Fatal("refusing DNS probe outside exact loopback-only resolver configuration")
	}
	if os.Geteuid() == 0 {
		t.Fatal("native CLI must run without root")
	}
	binary := os.Getenv("AGYN_NATIVE_DNS_BINARY")
	if binary != "/agyn/bin/claude" {
		t.Fatal("explicit installed native binary required")
	}

	root := t.TempDir()
	home, work := filepath.Join(root, "home"), filepath.Join(root, "workspace")
	for _, dir := range []string{home, work} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "", "CLAUDE_CODE_OAUTH_TOKEN": "", "ANTHROPIC_BASE_URL": "",
		"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "", "CLAUDE_CODE_USE_FOUNDRY": "",
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "http_proxy": "", "https_proxy": "", "all_proxy": "", "NO_PROXY": "", "no_proxy": "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
	} {
		t.Setenv(key, value)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeState(); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "claude-no-tools")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec \"$AGYN_NATIVE_DNS_BINARY\" --tools \"\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}

	var intercepted, bypassed atomic.Int32
	endpoint := func(address string, port int, status int, count *atomic.Int32) *httptest.Server {
		listener, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", address, port))
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 4<<20))
			if r.URL.Path == "/v1/messages" {
				count.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", "false")
			w.WriteHeader(status)
			kind := "invalid_request_error"
			if status == 401 {
				kind = "authentication_error"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": "local DNS fixture"}})
		}))
		_ = s.Listener.Close()
		s.Listener = listener
		s.Start()
		t.Cleanup(s.Close)
		return s
	}
	primary := endpoint("127.0.0.11", 0, 400, &intercepted)
	port := primary.Listener.Addr().(*net.TCPAddr).Port
	endpoint("127.0.0.12", port, 401, &bypassed)
	first := nativeDNSFixture(t, "127.0.0.2", [4]byte{127, 0, 0, 11}, 300*time.Millisecond, mode == "unavailable")
	second := nativeDNSFixture(t, "127.0.0.3", [4]byte{127, 0, 0, 12}, 0, false)
	deadline := 45 * time.Second
	if mode == "unavailable" {
		deadline = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		t.Fatal("native version probe failed")
	}
	if strings.TrimSpace(string(version)) != "2.1.225 (Claude Code)" {
		t.Fatal("unexpected native version")
	}
	client, err := claude.Start(ctx, claude.Options{BinaryPath: shim, WorkDir: work, Model: "sonnet", MaxTurns: 1, Stderr: io.Discard,
		Env: []string{fmt.Sprintf("ANTHROPIC_BASE_URL=http://a2a-native-dns.test:%d", port), "ANTHROPIC_API_KEY=local-dns-fixture-not-a-provider-key"}})
	if err != nil {
		t.Fatal("native initialization failed")
	}
	defer client.Close()
	result, turnErr := client.Turn(ctx, claude.TurnParams{Prompt: "Return OK."}, nil)
	status := 0
	if result != nil && result.APIErrorStatus != nil {
		status = *result.APIErrorStatus
	}
	evidence := map[string]any{"mode": mode, "primary_queries": first.Load(), "secondary_queries": second.Load(), "intercepted_messages": intercepted.Load(), "bypassed_messages": bypassed.Load(), "native_api_status": status, "turn_returned_error": turnErr != nil, "native_is_error": result != nil && result.IsError, "provider_credentials": false}
	encoded, _ := json.Marshal(evidence)
	t.Logf("native_dns_result=%s", encoded)
	if first.Load() == 0 {
		t.Fatal("native process did not query the interceptor DNS")
	}
	switch mode {
	case "mixed":
		if second.Load() == 0 || bypassed.Load() == 0 || intercepted.Load() != 0 || status != 401 {
			t.Fatal("mixed resolver did not reproduce the native interception bypass")
		}
	case "single":
		if second.Load() != 0 || bypassed.Load() != 0 || intercepted.Load() == 0 || status != 400 {
			t.Fatal("single resolver did not preserve the intercepted route")
		}
	case "unavailable":
		if second.Load() != 0 || bypassed.Load() != 0 || intercepted.Load() != 0 || (turnErr == nil && (result == nil || !result.IsError)) {
			t.Fatal("unavailable interceptor was not fail-closed")
		}
	}
}

func nativeDNSFixture(t *testing.T, address string, answer [4]byte, latency time.Duration, drop bool) *atomic.Int32 {
	t.Helper()
	conn, err := net.ListenPacket("udp4", address+":53")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		buffer := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			var request dnsmessage.Message
			if request.Unpack(buffer[:n]) != nil || len(request.Questions) != 1 {
				continue
			}
			question := request.Questions[0]
			target := question.Name.String() == "a2a-native-dns.test."
			if target {
				queries.Add(1)
			}
			if target && drop {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: request.ID, Response: true, RecursionDesired: request.RecursionDesired, RecursionAvailable: true}, Questions: request.Questions}
			if !target {
				response.RCode = dnsmessage.RCodeNameError
			} else if question.Type == dnsmessage.TypeA {
				response.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 0}, Body: &dnsmessage.AResource{A: answer}}}
			}
			payload, err := response.Pack()
			if err != nil {
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				select {
				case <-time.After(latency):
					_, _ = conn.WriteTo(payload, peer)
				case <-ctx.Done():
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); _ = conn.Close(); workers.Wait() })
	return &queries
}
