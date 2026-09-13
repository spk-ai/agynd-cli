package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeInitScriptsClient struct {
	responses []*agentsv1.ListInitScriptsResponse
	requests  []*agentsv1.ListInitScriptsRequest
	err       error
}

func (f *fakeInitScriptsClient) ListInitScripts(_ context.Context, in *agentsv1.ListInitScriptsRequest, _ ...grpc.CallOption) (*agentsv1.ListInitScriptsResponse, error) {
	f.requests = append(f.requests, in)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) == 0 {
		return &agentsv1.ListInitScriptsResponse{}, nil
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, nil
}

func TestInitScriptFromProtoValid(t *testing.T) {
	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	proto := &agentsv1.InitScript{
		Meta: &agentsv1.EntityMeta{
			Id:        "script-1",
			CreatedAt: timestamppb.New(createdAt),
		},
		Script:      "echo ok",
		Description: " setup description ",
	}
	script, err := initScriptFromProto(proto)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if script.ID != "script-1" {
		t.Fatalf("unexpected id: %s", script.ID)
	}
	if !script.CreatedAt.Equal(createdAt) {
		t.Fatalf("unexpected created at: %v", script.CreatedAt)
	}
	if script.Script != "echo ok" {
		t.Fatalf("unexpected script: %q", script.Script)
	}
	if script.Description != "setup description" {
		t.Fatalf("unexpected description: %q", script.Description)
	}
}

func TestInitScriptFromProtoMissingFields(t *testing.T) {
	createdAt := timestamppb.New(time.Now())
	validMeta := &agentsv1.EntityMeta{Id: "script-1", CreatedAt: createdAt}
	valid := &agentsv1.InitScript{Meta: validMeta, Script: "echo ok"}

	tests := []struct {
		name   string
		script *agentsv1.InitScript
	}{
		{name: "nil", script: nil},
		{name: "missing-meta", script: &agentsv1.InitScript{Script: valid.Script}},
		{name: "missing-id", script: &agentsv1.InitScript{Meta: &agentsv1.EntityMeta{CreatedAt: createdAt}, Script: valid.Script}},
		{name: "missing-created-at", script: &agentsv1.InitScript{Meta: &agentsv1.EntityMeta{Id: validMeta.Id}, Script: valid.Script}},
		{name: "missing-script", script: &agentsv1.InitScript{Meta: validMeta}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := initScriptFromProto(test.script)
			if err == nil {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestListInitScriptsOrdersByCreatedAtThenID(t *testing.T) {
	firstCreated := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	secondCreated := time.Date(2024, 6, 1, 11, 0, 0, 0, time.UTC)
	resp1 := &agentsv1.ListInitScriptsResponse{
		InitScripts: []*agentsv1.InitScript{
			{
				Meta:   &agentsv1.EntityMeta{Id: "c", CreatedAt: timestamppb.New(secondCreated)},
				Script: "echo third",
			},
			{
				Meta:   &agentsv1.EntityMeta{Id: "b", CreatedAt: timestamppb.New(firstCreated)},
				Script: "echo second",
			},
		},
		NextPageToken: "next",
	}
	resp2 := &agentsv1.ListInitScriptsResponse{
		InitScripts: []*agentsv1.InitScript{
			{
				Meta:   &agentsv1.EntityMeta{Id: "a", CreatedAt: timestamppb.New(firstCreated)},
				Script: "echo first",
			},
		},
	}
	fake := &fakeInitScriptsClient{responses: []*agentsv1.ListInitScriptsResponse{resp1, resp2}}
	scripts, err := listInitScripts(context.Background(), fake, "agent-1", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(fake.requests))
	}
	if fake.requests[0].GetAgentId() != "agent-1" || fake.requests[0].GetPageSize() != initScriptsPageSize || fake.requests[0].GetPageToken() != "" {
		t.Fatalf("unexpected first request: %+v", fake.requests[0])
	}
	if fake.requests[1].GetPageToken() != "next" {
		t.Fatalf("unexpected next page token: %q", fake.requests[1].GetPageToken())
	}
	if len(scripts) != 3 {
		t.Fatalf("unexpected script count: %d", len(scripts))
	}
	if scripts[0].ID != "a" || scripts[1].ID != "b" || scripts[2].ID != "c" {
		t.Fatalf("unexpected order: %v", []string{scripts[0].ID, scripts[1].ID, scripts[2].ID})
	}
}

func TestRunInitScriptsExecutesInOrder(t *testing.T) {
	workDir := t.TempDir()
	outputPath := filepath.Join(workDir, "order.txt")
	firstCreated := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	secondCreated := time.Date(2024, 6, 1, 11, 0, 0, 0, time.UTC)
	fake := &fakeInitScriptsClient{responses: []*agentsv1.ListInitScriptsResponse{{
		InitScripts: []*agentsv1.InitScript{
			{
				Meta:   &agentsv1.EntityMeta{Id: "later", CreatedAt: timestamppb.New(secondCreated)},
				Script: "printf 'second\n' >> order.txt",
			},
			{
				Meta:   &agentsv1.EntityMeta{Id: "early", CreatedAt: timestamppb.New(firstCreated)},
				Script: "printf 'first\n' >> order.txt",
			},
		},
	}}}
	if err := runInitScripts(context.Background(), fake, "agent-1", "", workDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("expected output file to exist, got %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("unexpected output: %q", string(data))
	}
}

func TestExecuteInitScriptNonZeroLogsAndContinues(t *testing.T) {
	var buffer bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()

	captureOutput := captureStdoutStderr(t)

	script := initScript{
		ID:          "script-err",
		CreatedAt:   time.Now(),
		Description: "setup step",
		Script:      "echo ok; echo bad 1>&2; exit 2",
	}
	if err := executeInitScript(context.Background(), script, t.TempDir(), false); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	output := captureOutput()
	if !strings.Contains(output, "ok") || !strings.Contains(output, "bad") {
		t.Fatalf("unexpected command output: %q", output)
	}
	logOutput := buffer.String()
	if !strings.Contains(logOutput, "running init script script-err: setup step") {
		t.Fatalf("unexpected start log output: %q", logOutput)
	}
	if !strings.Contains(logOutput, "init script script-err exited with code 2") {
		t.Fatalf("unexpected exit log output: %q", logOutput)
	}
}

func TestRequiredInitScriptsStopBeforeLaterScripts(t *testing.T) {
	for _, mode := range []string{"true", "false", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AGYN_INIT_SCRIPTS_REQUIRED", mode)
			workDir := t.TempDir()
			created := time.Now()
			client := &fakeInitScriptsClient{responses: []*agentsv1.ListInitScriptsResponse{{
				InitScripts: []*agentsv1.InitScript{
					{Meta: &agentsv1.EntityMeta{Id: "failed", CreatedAt: timestamppb.New(created)}, Script: "exit 17"},
					{Meta: &agentsv1.EntityMeta{Id: "later", CreatedAt: timestamppb.New(created.Add(time.Second))}, Script: "touch later"},
				},
			}}}
			err := runInitScripts(context.Background(), client, "agent", "", workDir)
			_, statErr := os.Stat(filepath.Join(workDir, "later"))
			if mode == "false" {
				if err != nil || statErr != nil {
					t.Fatalf("optional scripts should retain continuation: err=%v stat=%v", err, statErr)
				}
			} else {
				if err == nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("required or invalid mode must stop setup: err=%v stat=%v", err, statErr)
				}
				if mode == "true" && !strings.Contains(err.Error(), "exit code 17") {
					t.Fatalf("missing failure context: %v", err)
				}
				if mode == "invalid" && len(client.requests) != 0 {
					t.Fatal("invalid mode should fail before fetching or executing scripts")
				}
			}
		})
	}
}

func TestInitScriptCancellationIsNotAnOptionalExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := executeInitScript(ctx, initScript{ID: "cancel", Script: "exec sleep 30"}, t.TempDir(), false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
}

func captureStdoutStderr(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("expected pipe creation to succeed, got %v", err)
	}
	previousStdout := os.Stdout
	previousStderr := os.Stderr
	os.Stdout = writer
	os.Stderr = writer
	closed := false

	restore := func() {
		os.Stdout = previousStdout
		os.Stderr = previousStderr
	}

	t.Cleanup(func() {
		restore()
		if closed {
			return
		}
		_ = writer.Close()
		_ = reader.Close()
	})

	return func() string {
		restore()
		if err := writer.Close(); err != nil {
			t.Fatalf("expected pipe close to succeed, got %v", err)
		}
		output, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("expected pipe read to succeed, got %v", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("expected pipe close to succeed, got %v", err)
		}
		closed = true
		return string(output)
	}
}
