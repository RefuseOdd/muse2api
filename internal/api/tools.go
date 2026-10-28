package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const toolProtocolHead = `You can call the following tools when they help complete the user's task.
If you need to call a tool, output only a JSON code block in this format:

` + "```json" + `
{"name": "<tool name>", "arguments": {<arguments>}}
` + "```" + `

If you need to call multiple tools, output a JSON array of objects.
If no tool is needed, answer normally in natural language.

Available tools:
`

var (
	toolFenceRE  = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*(.*?)```")
	toolJSONRE   = regexp.MustCompile(`(?s)(\{.*\}|\[.*\])`)
	toolPrefixRE = regexp.MustCompile(`(?s)^(?:` + "```" + `)?(?:json|JSON)?\s*`)
)

func describeParams(params any) string {
	m, ok := params.(map[string]any)
	if !ok {
		return "      (no parameters)"
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) == 0 {
		return "      (no parameters)"
	}
	required := map[string]bool{}
	if list, ok := m["required"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		spec, _ := props[name].(map[string]any)
		typ := "any"
		if spec != nil {
			if s, ok := spec["type"].(string); ok && s != "" {
				typ = s
			}
		}
		req := "optional"
		if required[name] {
			req = "required"
		}
		desc := ""
		if spec != nil {
			if s, ok := spec["description"].(string); ok && s != "" {
				desc = " " + s
			}
		}
		lines = append(lines, fmt.Sprintf("      - %s (%s, %s)%s", name, typ, req, desc))
	}
	return strings.Join(lines, "\n")
}

func buildToolsPrompt(tools []any) string {
	if len(tools) == 0 {
		return ""
	}
	var items []string
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn := t
		if nested, ok := t["function"].(map[string]any); ok {
			fn = nested
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		suffix := ""
		if desc != "" {
			suffix = " - " + desc
		}
		items = append(items, fmt.Sprintf("%d. %s%s\n   Parameters:\n%s", len(items)+1, name, suffix, describeParams(fn["parameters"])))
	}
	if len(items) == 0 {
		return ""
	}
	return toolProtocolHead + strings.Join(items, "\n") + "\n"
}

func asToolCalls(obj any) []map[string]any {
	raw, ok := obj.([]any)
	if !ok {
		raw = []any{obj}
	}
	if len(raw) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		name, _ := m["tool"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		if name == "" {
			name, _ = m["function"].(string)
		}
		if name == "" {
			return nil
		}
		args := m["arguments"]
		if args == nil {
			args = m["parameters"]
		}
		if args == nil {
			args = m["args"]
		}
		if args == nil {
			args = map[string]any{}
		}
		argsStr, ok := args.(string)
		if !ok {
			argsStr, _ = jsonMarshalNoEscape(args)
		}
		out = append(out, map[string]any{
			"id":   "call_" + randomHex(20),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": argsStr,
			},
		})
	}
	return out
}

// parseToolCalls extracts tool calls from model text. It returns the calls and
// the remaining visible text; unsupported shapes are left untouched.
func parseToolCalls(text string) ([]map[string]any, string) {
	if text == "" {
		return nil, text
	}
	matches := toolFenceRE.FindAllStringSubmatchIndex(text, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		body := text[m[2]:m[3]]
		var parsed any
		if jsonUnmarshalStrict(body, &parsed) != nil {
			continue
		}
		if calls := asToolCalls(parsed); calls != nil {
			rest := strings.TrimSpace(text[:m[0]] + text[m[1]:])
			return calls, rest
		}
	}

	stripped := strings.TrimSpace(text)
	clean := strings.Trim(toolPrefixRE.ReplaceAllString(stripped, ""), "`")
	clean = strings.TrimSpace(clean)
	if strings.HasPrefix(clean, "{") || strings.HasPrefix(clean, "[") {
		var parsed any
		if jsonUnmarshalStrict(clean, &parsed) == nil {
			if calls := asToolCalls(parsed); calls != nil {
				return calls, ""
			}
		}
	}

	if m := toolJSONRE.FindStringIndex(text); m != nil {
		body := strings.TrimSpace(text[m[0]:m[1]])
		var parsed any
		if jsonUnmarshalStrict(body, &parsed) == nil {
			if calls := asToolCalls(parsed); calls != nil {
				rest := strings.TrimSpace(text[:m[0]] + text[m[1]:])
				if strings.TrimSpace(strings.Trim(toolPrefixRE.ReplaceAllString(rest, ""), "`")) == "" {
					rest = ""
				}
				return calls, rest
			}
		}
	}
	return nil, text
}

func jsonUnmarshalStrict(data string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	return dec.Decode(dst)
}
