package tools

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"charm.land/fantasy"
)

//go:embed memory.md
var memoryDescription string

const MemoryWriteToolName = "memory_write"

// memoryNameRe validates memory slugs: lowercase kebab-case, 1-64 chars.
var memoryNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

const (
	maxMemoryContentBytes = 4096
	maxMemoryDescription  = 200
	maxMemoryFiles        = 100
	memoryIndexFileName   = "MEMORY.md"
)

type MemoryWriteParams struct {
	Action      string `json:"action" description:"save (create or overwrite) or delete"`
	Name        string `json:"name" description:"Memory slug: lowercase letters, digits, hyphens (e.g. build-commands)"`
	Description string `json:"description,omitempty" description:"One-line summary shown in the memory index (required for save)"`
	Content     string `json:"content,omitempty" description:"The memory body in markdown (required for save, max 4096 bytes)"`
}

// NewMemoryTool returns the memory_write tool. dataDir is the absolute
// project data directory (Options.DataDirectory).
func NewMemoryTool(dataDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryWriteToolName,
		memoryDescription,
		func(ctx context.Context, params MemoryWriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			switch params.Action {
			case "save", "delete":
			default:
				return fantasy.NewTextErrorResponse("action must be save or delete"), nil
			}

			if !memoryNameRe.MatchString(params.Name) {
				return fantasy.NewTextErrorResponse("name must be lowercase letters, digits, and hyphens only (1-64 chars, e.g. build-commands)"), nil
			}

			memoryDir := filepath.Join(dataDir, "memory")
			path := filepath.Join(memoryDir, params.Name+".md")

			switch params.Action {
			case "save":
				if params.Description == "" {
					return fantasy.NewTextErrorResponse("description is required for save"), nil
				}
				if strings.ContainsAny(params.Description, "\n\r") {
					return fantasy.NewTextErrorResponse("description must be a single line"), nil
				}
				if len(params.Description) > maxMemoryDescription {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("description must be at most %d characters", maxMemoryDescription)), nil
				}
				if params.Content == "" {
					return fantasy.NewTextErrorResponse("content is required for save"), nil
				}
				if len(params.Content) > maxMemoryContentBytes {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("content must be at most %d bytes", maxMemoryContentBytes)), nil
				}

				if err := os.MkdirAll(memoryDir, 0o755); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("create memory directory: %w", err)
				}

				if _, err := os.Stat(path); os.IsNotExist(err) {
					count, err := countMemoryFiles(memoryDir)
					if err != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("count memory files: %w", err)
					}
					if count >= maxMemoryFiles {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("memory limit of %d files reached; delete or consolidate existing memories first", maxMemoryFiles)), nil
					}
				} else if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("stat memory file: %w", err)
				}

				body := formatMemoryFile(params.Description, params.Content)
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("write memory file: %w", err)
				}

				if err := regenerateMemoryIndex(memoryDir); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("regenerate memory index: %w", err)
				}

				return fantasy.NewTextResponse(fmt.Sprintf(
					`Saved memory "%s". The memory index in the system prompt refreshes on the next crush launch; rely on this conversation for the current session.`,
					params.Name,
				)), nil

			case "delete":
				if err := os.Remove(path); err != nil {
					if os.IsNotExist(err) {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("no memory named %s", params.Name)), nil
					}
					return fantasy.ToolResponse{}, fmt.Errorf("delete memory file: %w", err)
				}

				if err := regenerateMemoryIndex(memoryDir); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("regenerate memory index: %w", err)
				}

				return fantasy.NewTextResponse(fmt.Sprintf(`Deleted memory "%s".`, params.Name)), nil
			}

			return fantasy.NewTextErrorResponse("action must be save or delete"), nil
		},
	)
}

func formatMemoryFile(description, content string) string {
	return fmt.Sprintf("---\ndescription: %s\n---\n\n%s", description, content)
}

func countMemoryFiles(memoryDir string) (int, error) {
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == memoryIndexFileName || !strings.HasSuffix(name, ".md") {
			continue
		}
		count++
	}
	return count, nil
}

// regenerateMemoryIndex rebuilds MEMORY.md from a directory scan of
// individual memory files. MEMORY.md is never the source of truth.
func regenerateMemoryIndex(memoryDir string) error {
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type memoryEntry struct {
		name        string
		description string
	}
	var memories []memoryEntry

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == memoryIndexFileName || !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		b, err := os.ReadFile(filepath.Join(memoryDir, name))
		if err != nil {
			return err
		}
		desc := extractMemoryDescription(string(b), slug)
		memories = append(memories, memoryEntry{name: slug, description: desc})
	}

	slices.SortFunc(memories, func(a, b memoryEntry) int {
		return strings.Compare(a.name, b.name)
	})

	var sb strings.Builder
	sb.WriteString("# Memory index\n\n")
	for _, m := range memories {
		fmt.Fprintf(&sb, "- %s: %s\n", m.name, m.description)
	}

	return os.WriteFile(filepath.Join(memoryDir, memoryIndexFileName), []byte(sb.String()), 0o644)
}

// extractMemoryDescription returns the description: frontmatter value,
// or the slug if the file has no parseable frontmatter.
func extractMemoryDescription(content, slug string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return slug
	}
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			break
		}
		if after, ok := strings.CutPrefix(line, "description:"); ok {
			desc := strings.TrimSpace(after)
			if desc != "" {
				return desc
			}
			break
		}
	}
	return slug
}
