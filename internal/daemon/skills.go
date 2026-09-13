package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"google.golang.org/grpc"
)

const skillsPageSize int32 = 100

// The file every agent CLI with a skills feature looks for inside a skill's
// directory. A skill written as a plain file is not discovered at all.
const skillFileName = "SKILL.md"

// The name is the directory name, so the Agents service constrains it to a slug.
// Checked again here: a skill that cannot be a directory is skipped, not written.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

type skillsClient interface {
	ListSkills(ctx context.Context, in *agentsv1.ListSkillsRequest, opts ...grpc.CallOption) (*agentsv1.ListSkillsResponse, error)
}

type skill struct {
	ID          string
	Name        string
	Description string
	Body        string
	CreatedAt   time.Time
}

func listSkills(ctx context.Context, client skillsClient, agentID string) ([]skill, error) {
	if client == nil {
		return nil, fmt.Errorf("skills client is required")
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, fmt.Errorf("agent id is required")
	}
	var skills []skill
	pageToken := ""
	for {
		resp, err := client.ListSkills(ctx, &agentsv1.ListSkillsRequest{
			AgentId:   agentID,
			PageSize:  skillsPageSize,
			PageToken: pageToken,
		})
		if err != nil {
			return nil, err
		}
		for _, entry := range resp.GetSkills() {
			parsed, err := skillFromProto(entry)
			if err != nil {
				// One malformed skill is not worth an agent that never starts.
				log.Printf("skipping skill %s: %v", entry.GetMeta().GetId(), err)
				continue
			}
			skills = append(skills, parsed)
		}
		pageToken = strings.TrimSpace(resp.GetNextPageToken())
		if pageToken == "" {
			break
		}
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].CreatedAt.Equal(skills[j].CreatedAt) {
			return skills[i].ID < skills[j].ID
		}
		return skills[i].CreatedAt.Before(skills[j].CreatedAt)
	})
	return skills, nil
}

func skillFromProto(entry *agentsv1.Skill) (skill, error) {
	if entry == nil {
		return skill{}, fmt.Errorf("skill is required")
	}
	meta := entry.GetMeta()
	if meta == nil {
		return skill{}, fmt.Errorf("skill meta is required")
	}
	id := strings.TrimSpace(meta.GetId())
	if id == "" {
		return skill{}, fmt.Errorf("skill id is required")
	}
	createdAt := meta.GetCreatedAt()
	if createdAt == nil {
		return skill{}, fmt.Errorf("skill created at is required")
	}
	name := strings.TrimSpace(entry.GetName())
	if err := validateSkillName(name); err != nil {
		return skill{}, err
	}
	description := singleLine(entry.GetDescription())
	if description == "" {
		return skill{}, fmt.Errorf("skill description is required")
	}
	body := entry.GetBody()
	if strings.TrimSpace(body) == "" {
		return skill{}, fmt.Errorf("skill body is required")
	}
	return skill{
		ID:          id,
		Name:        name,
		Description: description,
		Body:        body,
		CreatedAt:   createdAt.AsTime(),
	}, nil
}

// writeSkills places each skill as its own directory holding a SKILL.md.
func writeSkills(sdk string, skills []skill) (string, error) {
	if len(skills) == 0 {
		return "", nil
	}
	skillsDir, err := skillsDirForSDK(sdk)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return "", fmt.Errorf("create skills dir: %w", err)
	}
	seen := make(map[string]struct{}, len(skills))
	for _, entry := range skills {
		if _, ok := seen[entry.Name]; ok {
			log.Printf("skipping skill %s: name %q is already taken", entry.ID, entry.Name)
			continue
		}
		seen[entry.Name] = struct{}{}
		dir := filepath.Join(skillsDir, entry.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create skill dir %s: %w", entry.Name, err)
		}
		path := filepath.Join(dir, skillFileName)
		if err := os.WriteFile(path, []byte(skillDocument(entry)), 0o600); err != nil {
			return "", fmt.Errorf("write skill %s: %w", entry.Name, err)
		}
	}
	return skillsDir, nil
}

// The stored body carries no front matter, so the platform writes it: name and
// description are what a CLI reads before it decides to open the body.
func skillDocument(entry skill) string {
	var doc strings.Builder
	doc.WriteString("---\n")
	doc.WriteString("name: " + yamlString(entry.Name) + "\n")
	doc.WriteString("description: " + yamlString(entry.Description) + "\n")
	doc.WriteString("---\n\n")
	doc.WriteString(strings.TrimSpace(entry.Body))
	doc.WriteString("\n")
	return doc.String()
}

// A description is prose and may hold anything a colon or a quote can mean in
// YAML, so it is emitted double-quoted rather than bare.
func yamlString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func skillsDirForSDK(sdk string) (string, error) {
	switch sdk {
	case SDKCodex:
		// Codex reads $CODEX_HOME/skills too and marks it deprecated.
		return filepath.Join(codexHomeEnv(), ".agents", "skills"), nil
	case SDKClaude:
		directory, err := claudeConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(directory, "skills"), nil
	default:
		// agn has no skills feature: its skills reach the model in the system
		// prompt and nothing is written for them.
		return "", fmt.Errorf("sdk %q has no skills directory", sdk)
	}
}

func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if !skillNamePattern.MatchString(name) {
		return fmt.Errorf("skill name %q must match %s", name, skillNamePattern.String())
	}
	return nil
}

func buildSystemPrompt(systemPrompt string, skills []skill) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" && len(skills) == 0 {
		return ""
	}
	parts := make([]string, 0, len(skills)+1)
	if systemPrompt != "" {
		parts = append(parts, systemPrompt)
	}
	for _, entry := range skills {
		body := strings.TrimSpace(entry.Body)
		if body == "" {
			continue
		}
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n\n")
}
