package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"google.golang.org/grpc"
)

const initScriptsPageSize int32 = 100

type initScriptsClient interface {
	ListInitScripts(ctx context.Context, in *agentsv1.ListInitScriptsRequest, opts ...grpc.CallOption) (*agentsv1.ListInitScriptsResponse, error)
}

type initScript struct {
	ID          string
	CreatedAt   time.Time
	Script      string
	Description string
}

func runInitScripts(ctx context.Context, client initScriptsClient, agentID string, environmentID string, workDir string) error {
	required := false
	if value := strings.TrimSpace(os.Getenv("AGYN_INIT_SCRIPTS_REQUIRED")); value != "" {
		var err error
		required, err = strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("AGYN_INIT_SCRIPTS_REQUIRED must be a boolean")
		}
	}
	scripts, err := listInitScripts(ctx, client, agentID, environmentID)
	if err != nil {
		return err
	}
	for _, script := range scripts {
		if err := executeInitScript(ctx, script, workDir, required); err != nil {
			return err
		}
	}
	return nil
}

// listInitScripts returns the environment's scripts first, then the agent's,
// each in creation order. Names do not collide -- every script runs -- so the
// order is the whole contract.
func listInitScripts(ctx context.Context, client initScriptsClient, agentID string, environmentID string) ([]initScript, error) {
	if client == nil {
		return nil, fmt.Errorf("init scripts client is required")
	}
	agentID = strings.TrimSpace(agentID)
	environmentID = strings.TrimSpace(environmentID)
	if agentID == "" && environmentID == "" {
		return nil, fmt.Errorf("agent id or environment id is required")
	}
	var ordered []initScript
	if environmentID != "" {
		scripts, err := listInitScriptsForTarget(ctx, client, &agentsv1.ListInitScriptsRequest{EnvironmentId: environmentID})
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, scripts...)
	}
	if agentID != "" {
		scripts, err := listInitScriptsForTarget(ctx, client, &agentsv1.ListInitScriptsRequest{AgentId: agentID})
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, scripts...)
	}
	return ordered, nil
}

func listInitScriptsForTarget(ctx context.Context, client initScriptsClient, req *agentsv1.ListInitScriptsRequest) ([]initScript, error) {
	var scripts []initScript
	pageToken := ""
	for {
		resp, err := client.ListInitScripts(ctx, &agentsv1.ListInitScriptsRequest{
			AgentId:       req.GetAgentId(),
			EnvironmentId: req.GetEnvironmentId(),
			PageSize:      initScriptsPageSize,
			PageToken:     pageToken,
		})
		if err != nil {
			return nil, err
		}
		for _, script := range resp.GetInitScripts() {
			parsed, err := initScriptFromProto(script)
			if err != nil {
				return nil, err
			}
			scripts = append(scripts, parsed)
		}
		pageToken = strings.TrimSpace(resp.GetNextPageToken())
		if pageToken == "" {
			break
		}
	}
	sort.Slice(scripts, func(i, j int) bool {
		if scripts[i].CreatedAt.Equal(scripts[j].CreatedAt) {
			return scripts[i].ID < scripts[j].ID
		}
		return scripts[i].CreatedAt.Before(scripts[j].CreatedAt)
	})
	return scripts, nil
}

func initScriptFromProto(script *agentsv1.InitScript) (initScript, error) {
	if script == nil {
		return initScript{}, fmt.Errorf("init script is required")
	}
	meta := script.GetMeta()
	if meta == nil {
		return initScript{}, fmt.Errorf("init script meta is required")
	}
	id := strings.TrimSpace(meta.GetId())
	if id == "" {
		return initScript{}, fmt.Errorf("init script id is required")
	}
	createdAt := meta.GetCreatedAt()
	if createdAt == nil {
		return initScript{}, fmt.Errorf("init script created at is required")
	}
	rawScript := script.GetScript()
	if strings.TrimSpace(rawScript) == "" {
		return initScript{}, fmt.Errorf("init script script is required")
	}
	description := strings.TrimSpace(script.GetDescription())
	return initScript{
		ID:          id,
		CreatedAt:   createdAt.AsTime(),
		Script:      rawScript,
		Description: description,
	}, nil
}

func executeInitScript(ctx context.Context, script initScript, workDir string, required bool) error {
	logInitScriptStart(script)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", script.Script)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			log.Printf("init script %s exited with code %d", script.ID, exitErr.ExitCode())
			if required {
				return fmt.Errorf("required init script %s failed with exit code %d", script.ID, exitErr.ExitCode())
			}
			return nil
		}
		return fmt.Errorf("run init script %s: %w", script.ID, err)
	}
	return nil
}

func logInitScriptStart(script initScript) {
	if script.Description == "" {
		log.Printf("running init script %s", script.ID)
		return
	}
	log.Printf("running init script %s: %s", script.ID, script.Description)
}
