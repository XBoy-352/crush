package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// LeakedToolCall is a tool call recovered from DSML-style markup that a
// model leaked into its text stream instead of emitting as structured
// provider tool calls.
type LeakedToolCall struct {
	Name   string
	Params map[string]any
}

// Spaced DeepSeek DSML (observed on Charm Hyper): special tokens arrive
// as literal text with spaces around the pipe characters.
var (
	spacedInvokeOpenRe = regexp.MustCompile(
		`<\s*(?:\|\s*DSML\s*\|\s*)+(?:\|\s*DSML\s*\|\s*)*invoke\s+name="([^"]+)"\s*>`)
	spacedToolCallsWrapRe = regexp.MustCompile(
		`</?\s*(?:\|\s*DSML\s*\|\s*)+tool[_ ]?calls\s*/?>`)
)

// Compact DeepSeek DSML using fullwidth pipe delimiters:
// <｜tool calls｜><｜invoke:view｜> ... <｜/invoke｜><｜/tool calls｜>.
var compactInvokeRe = regexp.MustCompile(
	`<｜invoke:\s*([^｜]+)\s*｜>([\s\S]*?)<｜/invoke｜>`)

// Generic JSON tool-call blocks used by Qwen and Hermes-style models.
var genericToolCallRe = regexp.MustCompile(
	`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// Parameter elements inside invoke bodies, in both spaced and compact
// forms.
var (
	spacedParameterRe = regexp.MustCompile(
		`<\s*(?:\|\s*DSML\s*\|\s*)?parameter\s+name="([^"]+)"(?:\s+string="(true|false)")?\s*>` +
			`([\s\S]*?)<\s*/\s*(?:\|\s*DSML\s*\|\s*)?parameter\s*>`)
	compactParameterRe = regexp.MustCompile(
		`<｜parameter\s+name="([^"]+)"(?:\s+string="(true|false)")?\s*｜>([\s\S]*?)<｜/parameter｜>`)
)

// spacedInvokeClose matches the closing invoke tag in the spaced form,
// allowing the doubled pipe group the leaked markup exhibits.
var spacedInvokeCloseRe = regexp.MustCompile(
	`</\s*(?:\|\s*DSML\s*\|\s*)+(?:\|\s*DSML\s*\|\s*)*invoke\s*>`)

// maxLeakedMarkupScan caps the text scanned for leaked markup so
// pathological inputs cannot stall the stream on regex backtracking.
const maxLeakedMarkupScan = 1 << 20

// ExtractLeakedToolCalls scans model text for leaked DSML/XML tool-call
// markup and recovers the tool calls it references. Only calls naming a
// tool in availableTools are returned; everything else is treated as
// ordinary conversation about markup. The second return value is the
// text with all recovered markup removed, ready for display.
func ExtractLeakedToolCalls(text string, availableTools []string) ([]LeakedToolCall, string) {
	if !strings.Contains(text, "<") || len(text) > maxLeakedMarkupScan {
		return nil, text
	}
	allowed := make(map[string]struct{}, len(availableTools))
	for _, name := range availableTools {
		allowed[name] = struct{}{}
	}

	var calls []LeakedToolCall
	clean := text

	for {
		loc := spacedInvokeOpenRe.FindStringIndex(clean)
		if loc == nil {
			break
		}
		open := clean[loc[0]:loc[1]]
		name := strings.TrimSpace(spacedInvokeOpenRe.FindStringSubmatch(open)[1])
		if _, ok := allowed[name]; !ok {
			// Skip past this tag so unknown tools do not loop forever.
			clean = clean[:loc[0]] + clean[loc[1]:]
			continue
		}
		closeLoc := spacedInvokeCloseRe.FindStringIndex(clean[loc[1]:])
		if closeLoc == nil {
			clean = clean[:loc[0]] + clean[loc[1]:]
			continue
		}
		bodyStart := loc[1]
		bodyEnd := loc[1] + closeLoc[0]
		body := clean[bodyStart:bodyEnd]
		calls = append(calls, LeakedToolCall{Name: name, Params: parseDSMLParameters(body)})
		clean = clean[:loc[0]] + clean[bodyEnd+closeLoc[1]-closeLoc[0]:]
	}
	clean = spacedToolCallsWrapRe.ReplaceAllString(clean, "")

	for _, m := range compactInvokeRe.FindAllStringSubmatch(clean, -1) {
		name := strings.TrimSpace(m[1])
		if _, ok := allowed[name]; !ok {
			continue
		}
		calls = append(calls, LeakedToolCall{Name: name, Params: parseDSMLParameters(m[2])})
		clean = strings.ReplaceAll(clean, m[0], "")
	}
	clean = strings.NewReplacer("<｜tool calls｜>", "", "<｜/tool calls｜>", "").Replace(clean)

	for _, m := range genericToolCallRe.FindAllStringSubmatch(clean, -1) {
		call, ok := parseGenericToolCallJSON(m[1])
		if !ok {
			continue
		}
		if _, ok := allowed[call.Name]; !ok {
			continue
		}
		calls = append(calls, call)
		clean = strings.ReplaceAll(clean, m[0], "")
	}

	if len(calls) == 0 {
		return nil, text
	}
	return calls, clean
}

// parseGenericToolCallJSON parses the JSON body of a generic
// <tool_call> block, accepting either {"name": ..., "arguments": ...}
// or {"name": ..., "parameters": ...}.
func parseGenericToolCallJSON(raw string) (LeakedToolCall, bool) {
	var payload struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Params    map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return LeakedToolCall{}, false
	}
	if payload.Name == "" {
		return LeakedToolCall{}, false
	}
	args := payload.Arguments
	if args == nil {
		args = payload.Params
	}
	return LeakedToolCall{Name: payload.Name, Params: args}, true
}

// parseDSMLParameters extracts every <parameter> element from an invoke
// body and infers each value's type from the string attribute.
func parseDSMLParameters(body string) map[string]any {
	params := make(map[string]any)
	for _, re := range []*regexp.Regexp{spacedParameterRe, compactParameterRe} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			name := strings.TrimSpace(m[1])
			if m[2] == "false" {
				params[name] = inferTypedValue(m[3])
				continue
			}
			params[name] = m[3]
		}
	}
	return params
}

// inferTypedValue converts a raw string parameter into its native type:
// int, float, bool, falling back to the original string.
func inferTypedValue(raw string) any {
	value := strings.TrimSpace(raw)
	if i, err := strconv.Atoi(value); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	switch strings.ToLower(value) {
	case "true":
		return true
	case "false":
		return false
	}
	return raw
}

// MarshalLeakedToolCall serializes recovered parameters into the JSON
// input fantasy expects on StreamPart.ToolCallInput.
func MarshalLeakedToolCall(call LeakedToolCall) (string, error) {
	if call.Params == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(call.Params)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
