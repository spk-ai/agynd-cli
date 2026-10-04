// Command fakeclaude stands in for the Claude Code CLI in E2E and init-image
// tests. At start it records the user-scope MCP servers it would load and
// whether each HTTP server answered an MCP initialize, as the real CLI connects
// only then; it then completes the SDK's stream-json initialize handshake and
// waits for stdin to close. It never runs a turn.
//
// FAKE_CLAUDE_RECORD names the JSON file the record is written to.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	servers := declaredServers()
	answered := map[string]bool{}
	client := &http.Client{Timeout: 2 * time.Second}
	for name, raw := range servers {
		server, _ := raw.(map[string]any)
		url, _ := server["url"].(string)
		if server["type"] != "http" || url == "" {
			continue
		}
		body := []byte(`{"jsonrpc":"2.0","id":"fake-claude","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-claude","version":"0"}}}`)
		request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := client.Do(request)
		if err == nil {
			answered[name] = response.StatusCode == http.StatusOK
			_ = response.Body.Close()
		}
	}
	record, err := json.Marshal(map[string]any{"mcpServers": servers, "answered": answered})
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Getenv("FAKE_CLAUDE_RECORD"), record, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: write record: %v\n", err)
		os.Exit(3)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		var message struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if message.Type == "control_request" && message.Request.Subtype == "initialize" {
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q}}\n", message.RequestID)
		}
	}
}

// declaredServers reads mcpServers from the user state file the real CLI
// reads: CLAUDE_CONFIG_DIR/.claude.json when that is set, else HOME/.claude.json.
func declaredServers() map[string]any {
	path := filepath.Join(os.Getenv("HOME"), ".claude.json")
	if directory, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok {
		path = filepath.Join(directory, ".claude.json")
	}
	state := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	servers, _ := state["mcpServers"].(map[string]any)
	return servers
}
