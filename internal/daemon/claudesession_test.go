package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/platform"
	claude "github.com/agynio/claude-sdk-go"
	"github.com/google/uuid"
)

func persistentClaudeFixture(t *testing.T) (config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	state, mapping := filepath.Join(root, "state"), filepath.Join(root, "mapping")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", state)
	t.Setenv("AGYN_CLAUDE_SESSION_DIR", mapping)
	cfg := testConfig(SDKClaude)
	cfg.AgentInstanceID = uuid.New()
	return cfg, state, mapping
}

func nativeClaudeFixture(t *testing.T, state, workdir, id string) {
	t.Helper()
	directory := filepath.Join(state, "projects", "test-project")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"type": "user", "sessionId": id, "cwd": workdir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, id+".jsonl"), payload, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudePersistentStateConfigAndSkillsSurviveHomeReplacement(t *testing.T) {
	cfg, state, _ := persistentClaudeFixture(t)
	session, err := prepareClaudeSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := writeClaudeState(); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeSettings("", "", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkills(SDKClaude, []skill{{ID: "1", Name: "review", Description: "Review code", Body: "Review the current task."}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(state, ".claude.json"), filepath.Join(state, "settings.json"), filepath.Join(state, "skills", "review", "SKILL.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("relocated file missing: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote state into ephemeral HOME")
	}
	userPath := filepath.Join(state, ".claude.json")
	if err := os.WriteFile(userPath, []byte(`{"hasCompletedOnboarding":true,"mcpServers":{"existing":{"type":"http","url":"https://example.test/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	nativeClaudeFixture(t, state, cfg.WorkDir, session.ID)
	_ = session.Close()
	t.Setenv("HOME", t.TempDir())
	replacement, err := prepareClaudeSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.ID != session.ID || replacement.Transcript == "" {
		t.Fatal("replacing HOME changed the selected native session")
	}
	if err := writeClaudeState(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(userPath)
	if err != nil || !strings.Contains(string(data), "existing") {
		t.Fatal("existing native user state was discarded")
	}
}

func TestClaudePersistentStateRejectsInvalidConfigurationAndUserState(t *testing.T) {
	cfg, state, mapping := persistentClaudeFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "relative")
	if _, err := prepareClaudeSession(cfg); err == nil {
		t.Fatal("relative native state path was accepted")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", state)
	t.Setenv("AGYN_CLAUDE_SESSION_DIR", "")
	if _, err := prepareClaudeSession(cfg); err == nil {
		t.Fatal("empty opt-in silently disabled persistence")
	}
	t.Setenv("AGYN_CLAUDE_SESSION_DIR", mapping)
	holder := cfg
	holder.Mode = config.ModeHolder
	if _, err := prepareClaudeSession(holder); err == nil {
		t.Fatal("holder mode acquired unmanaged access to a persistent session")
	}
	session, err := prepareClaudeSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, body := range []string{"not json", "null", "[]"} {
		path := filepath.Join(state, ".claude.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeClaudeState(); err == nil {
			t.Fatal("corrupt persistent user state was silently reset")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != body {
			t.Fatal("corrupt persistent user state was overwritten")
		}
	}
}

func TestClaudeStartupFailureReleasesSessionOwnership(t *testing.T) {
	cfg, state, mapping := persistentClaudeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if daemon, err := New(ctx, cfg, "test"); err == nil {
		daemon.Close()
		t.Fatal("canceled startup returned a daemon")
	}
	payload, err := os.ReadFile(filepath.Join(mapping, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		ID string `json:"session_id"`
	}
	if err := json.Unmarshal(payload, &record); err != nil || record.ID == "" {
		t.Fatal("startup did not reserve its session identity")
	}
	nativeClaudeFixture(t, state, cfg.WorkDir, record.ID)
	session, err := prepareClaudeSession(cfg)
	if err != nil {
		t.Fatalf("failed constructor retained ownership: %v", err)
	}
	_ = session.Close()
}

func TestClaudeSessionMismatchIsTerminalBeforeReplyOrAck(t *testing.T) {
	cfg, _, _ := persistentClaudeFixture(t)
	session, err := prepareClaudeSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client := &fakeClaudeClient{result: &claude.TurnResult{SessionID: uuid.NewString(), Response: "done"}}
	threads := &fakeClaudeThreadsClient{}
	daemon := &Daemon{cfg: cfg, sdk: SDKClaude, claude: client, claudeSession: session,
		threads: platform.NewThreads(threads), agent: &agentsv1.Agent{FinalMessage: agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD}}
	err = daemon.handleClaudeMessage(context.Background(), platform.Message{ID: "message", ThreadID: "thread", Body: "work"})
	if !errors.Is(err, errClaudeSessionMismatch) || !isTerminalAgentProcessingError(err) {
		t.Fatalf("session mismatch could be retried: %v", err)
	}
	if client.turnCalls != 1 || len(threads.sendRequests) != 0 || len(threads.ackRequests) != 0 {
		t.Fatal("mismatched session published a reply or acknowledged the inbox")
	}
}

type closingClaudeClient struct {
	*fakeClaudeClient
	close func() error
}

func (c *closingClaudeClient) Close() error { return c.close() }

func TestClaudeCloseRetainsOwnershipUntilConfirmedClientClose(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			cfg, state, _ := persistentClaudeFixture(t)
			session, err := prepareClaudeSession(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			nativeClaudeFixture(t, state, cfg.WorkDir, session.ID)
			closes := 0
			client := &closingClaudeClient{close: func() error {
				closes++
				if other, err := prepareClaudeSession(cfg); err == nil {
					_ = other.Close()
					t.Fatal("ownership released before client Close completed")
				}
				if failed {
					return errors.New("close unconfirmed")
				}
				return nil
			}}
			daemon := &Daemon{claude: client, claudeSession: session}
			daemon.Close()
			daemon.Close()
			if closes != 1 {
				t.Fatal("repeated Close retried an ambiguous client shutdown")
			}
			other, err := prepareClaudeSession(cfg)
			if err == nil {
				_ = other.Close()
			}
			if (err != nil) != failed {
				t.Fatalf("ownership after close: %v", err)
			}
		})
	}
}
