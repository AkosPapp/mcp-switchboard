package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Some local models (the Qwen family behind Ollama or vLLM in particular) write
// their tool call as text in the model's native template instead of through the
// server's tool-call channel - most often inside the reasoning block, where the
// server's parser never looks. The stream then ends with finish "stop" and no
// tool call, and the run silently ends mid-task.
//
// recoverTextToolCalls is the fallback for that: when a completion produced no
// structured call, look for the two text formats in what it did produce and
// return calls for tools that were actually offered. A name that was not
// offered is ignored, so prose that merely mentions the format cannot invoke
// anything.
//
//	Qwen XML:  <tool_call><function=NAME><parameter=KEY>VALUE</parameter>...</function></tool_call>
//	Hermes:    <tool_call>{"name": "NAME", "arguments": {...}}</tool_call>
var (
	xmlFuncRe  = regexp.MustCompile(`(?s)<function=([^>\s]+)\s*>(.*?)(?:</function>|</tool_call>|$)`)
	xmlParamRe = regexp.MustCompile(`(?s)<parameter=([^>\s]+)\s*>(.*?)(?:</parameter>|<parameter=|</function>|$)`)
	jsonCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*(?:</tool_call>|$)`)
)

// recoverTextToolCalls looks in text first (what the model said to the user),
// then in thinking. Calls come back in the order they appear, at most 16.
func recoverTextToolCalls(text, thinking string, tools []Tool) []ToolCall {
	if len(tools) == 0 {
		return nil
	}
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	for _, src := range []string{text, thinking} {
		if calls := parseTextCalls(src, byName); len(calls) > 0 {
			return calls
		}
	}
	return nil
}

func parseTextCalls(src string, byName map[string]Tool) []ToolCall {
	if !strings.Contains(src, "<function=") && !strings.Contains(src, "<tool_call>") {
		return nil
	}
	type found struct {
		pos  int
		call ToolCall
	}
	var all []found

	for _, m := range xmlFuncRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		tool, ok := lookupTool(byName, name)
		if !ok {
			continue
		}
		args := map[string]any{}
		for _, p := range xmlParamRe.FindAllStringSubmatch(src[m[4]:m[5]], -1) {
			args[p[1]] = coerceParam(tool, p[1], p[2])
		}
		all = append(all, found{m[0], ToolCall{Name: tool.Name, Arguments: args}})
	}
	for _, m := range jsonCallRe.FindAllStringSubmatchIndex(src, -1) {
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(src[m[2]:m[3]]), &c) != nil {
			continue
		}
		tool, ok := lookupTool(byName, c.Name)
		if !ok {
			continue
		}
		all = append(all, found{m[0], ToolCall{Name: tool.Name, Arguments: parseArgsRaw(c.Arguments)}})
	}

	// Both regexps can hit one block only if the JSON form contains "<function=",
	// which a real call never does; keep source order and cap the count.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].pos < all[j-1].pos; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	var out []ToolCall
	for i, f := range all {
		if i == 16 {
			break
		}
		f.call.ID = fmt.Sprintf("call_text_%d", i)
		out = append(out, f.call)
	}
	return out
}

// lookupTool accepts the provider-safe name exactly, or the dotted spelling of
// it (switchboard.todo.write for switchboard_todo_write), which is how the
// model sees hub tools described in prose.
func lookupTool(byName map[string]Tool, name string) (Tool, bool) {
	name = strings.TrimSpace(name)
	if t, ok := byName[name]; ok {
		return t, true
	}
	if t, ok := byName[strings.ReplaceAll(name, ".", "_")]; ok {
		return t, true
	}
	return Tool{}, false
}

// coerceParam turns a parameter's text into the type its schema declares. The
// XML form carries every value as text; a string stays a string and everything
// else is decoded as JSON, falling back to the raw text.
func coerceParam(tool Tool, key, raw string) any {
	// One leading and one trailing newline are template formatting, not data.
	raw = strings.TrimPrefix(raw, "\n")
	raw = strings.TrimSuffix(raw, "\n")

	want := ""
	if props, ok := tool.InputSchema["properties"].(map[string]any); ok {
		if p, ok := props[key].(map[string]any); ok {
			want, _ = p["type"].(string)
		}
	}
	if want == "string" {
		return raw
	}
	trimmed := strings.TrimSpace(raw)
	var v any
	if json.Unmarshal([]byte(trimmed), &v) == nil {
		if want == "" {
			// Unknown type: only trust JSON for structures and booleans/null, so
			// a bare word or number the model meant as text is not reinterpreted.
			switch v.(type) {
			case map[string]any, []any, bool, nil, float64:
				return v
			}
			return raw
		}
		return v
	}
	return raw
}
