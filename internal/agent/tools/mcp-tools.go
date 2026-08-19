package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
)

// whitelistDockerTools contains Docker MCP tools that don't require permission.
var whitelistDockerTools = []string{
	"mcp_docker_mcp-find",
	"mcp_docker_mcp-add",
	"mcp_docker_mcp-remove",
	"mcp_docker_mcp-config-set",
	"mcp_docker_code-mode",
}

// GetMCPTools gets all the currently available MCP tools.
func GetMCPTools(permissions permission.Service, cfg *config.ConfigStore, wd string) []*Tool {
	var result []*Tool
	for mcpName, tools := range mcp.Tools() {
		for _, tool := range tools {
			result = append(result, &Tool{
				mcpName:     mcpName,
				tool:        tool,
				permissions: permissions,
				workingDir:  wd,
				cfg:         cfg,
			})
		}
	}
	return result
}

// Tool is a tool from a MCP.
type Tool struct {
	mcpName         string
	tool            *mcp.Tool
	cfg             *config.ConfigStore
	permissions     permission.Service
	workingDir      string
	providerOptions fantasy.ProviderOptions
}

func (m *Tool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.providerOptions = opts
}

func (m *Tool) ProviderOptions() fantasy.ProviderOptions {
	return m.providerOptions
}

func (m *Tool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", m.mcpName, m.tool.Name)
}

func (m *Tool) MCP() string {
	return m.mcpName
}

func (m *Tool) MCPToolName() string {
	return m.tool.Name
}

func (m *Tool) Info() fantasy.ToolInfo {
	parameters := make(map[string]any)
	required := make([]string, 0)

	if input, ok := m.tool.InputSchema.(map[string]any); ok {
		if props, ok := input["properties"].(map[string]any); ok {
			if resolved, ok := inlineSchemaRefs(props, schemaDefs(input), nil).(map[string]any); ok {
				parameters = resolved
			}
		}
		if req, ok := input["required"].([]any); ok {
			// Convert []any -> []string when elements are strings
			for _, v := range req {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		} else if reqStr, ok := input["required"].([]string); ok {
			// Handle case where it's already []string
			required = reqStr
		}
	}

	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

// schemaDefs returns the reusable subschema block of a JSON Schema, supporting
// both the 2020-12 name ($defs) and the draft-07 one (definitions).
func schemaDefs(input map[string]any) map[string]any {
	if defs, ok := input["$defs"].(map[string]any); ok {
		return defs
	}
	defs, _ := input["definitions"].(map[string]any)
	return defs
}

// inlineSchemaRefs replaces every $ref with a copy of the subschema it points
// at, so the result stands alone.
//
// Info only forwards a tool's "properties" to the provider and drops the rest
// of the schema, including the $defs block that $refs resolve against. A $ref
// that survived that trim would point at something no longer in the payload;
// strict validators (xAI) reject the whole request with a 400, which takes down
// every other tool in the same call. A $ref that cannot be resolved is dropped
// rather than forwarded, so a dangling pointer never reaches the provider.
//
// seen carries the $defs names already being expanded on this path, so a
// self-referential schema stops instead of recursing forever.
func inlineSchemaRefs(node any, defs map[string]any, seen []string) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			if k != "$ref" {
				out[k] = inlineSchemaRefs(v, defs, seen)
			}
		}

		ref, ok := n["$ref"].(string)
		if !ok {
			return out
		}
		name, ok := defRefName(ref)
		if !ok {
			return out
		}
		target, ok := defs[name]
		if !ok || slices.Contains(seen, name) {
			return out
		}
		resolved, ok := inlineSchemaRefs(target, defs, slices.Concat(seen, []string{name})).(map[string]any)
		if !ok {
			return out
		}
		// Keys alongside a $ref override the target's, per JSON Schema 2020-12.
		maps.Copy(resolved, out)
		return resolved
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inlineSchemaRefs(v, defs, seen)
		}
		return out
	default:
		return node
	}
}

// defRefName returns the $defs key a local reference points at.
func defRefName(ref string) (string, bool) {
	for _, prefix := range []string{"#/$defs/", "#/definitions/"} {
		if name, ok := strings.CutPrefix(ref, prefix); ok && name != "" {
			return name, true
		}
	}
	return "", false
}

func (m *Tool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	// Skip permission for whitelisted Docker MCP tools.
	if !slices.Contains(whitelistDockerTools, params.Name) {
		permissionDescription := fmt.Sprintf("execute %s with the following parameters:", m.Info().Name)
		p, err := m.permissions.Request(
			ctx,
			permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  params.ID,
				Path:        m.workingDir,
				ToolName:    m.Info().Name,
				Action:      "execute",
				Description: permissionDescription,
				Params:      params.Input,
			},
		)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if !p {
			return NewPermissionDeniedResponse(), nil
		}
	}

	result, err := mcp.RunTool(ctx, m.cfg, m.mcpName, m.tool.Name, params.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	switch result.Type {
	case "image", "media":
		if !GetSupportsImagesFromContext(ctx) {
			modelName := GetModelNameFromContext(ctx)
			return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
		}

		var response fantasy.ToolResponse
		if result.Type == "image" {
			response = fantasy.NewImageResponse(result.Data, result.MediaType)
		} else {
			response = fantasy.NewMediaResponse(result.Data, result.MediaType)
		}
		response.Content = result.Content
		return response, nil
	default:
		return fantasy.NewTextResponse(result.Content), nil
	}
}
