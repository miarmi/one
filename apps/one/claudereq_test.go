package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// validSchema checks a JSON Schema node the way the Messages API is strict
// about it: objects have properties that are schemas, required names exist,
// no null where a schema belongs, types are strings or arrays of strings.
func validSchema(path string, v any, errs *[]string) {
	m, ok := v.(map[string]any)
	if !ok {
		*errs = append(*errs, path+": not an object")
		return
	}
	switch t := m["type"].(type) {
	case nil:
	case string:
	case []any:
		for _, x := range t {
			if _, ok := x.(string); !ok {
				*errs = append(*errs, path+": type list with a non-string")
			}
		}
	default:
		*errs = append(*errs, path+": bad type")
	}
	if props, has := m["properties"]; has {
		pm, ok := props.(map[string]any)
		if !ok {
			*errs = append(*errs, path+": properties is not an object")
		} else {
			for k, p := range pm {
				validSchema(path+"."+k, p, errs)
			}
		}
	}
	if req, has := m["required"]; has {
		rl, ok := req.([]any)
		if !ok {
			*errs = append(*errs, path+": required is not a list")
		} else {
			pm, _ := m["properties"].(map[string]any)
			for _, r := range rl {
				if _, found := pm[fmt.Sprint(r)]; !found {
					*errs = append(*errs, path+": required names missing property "+fmt.Sprint(r))
				}
			}
		}
	}
	if en, has := m["enum"]; has {
		if l, ok := en.([]any); !ok || len(l) == 0 {
			*errs = append(*errs, path+": empty or bad enum")
		}
	}
	if it, has := m["items"]; has {
		validSchema(path+"[]", it, errs)
	}
	for k, x := range m {
		if x == nil {
			*errs = append(*errs, path+"."+k+": null")
		}
	}
}

func TestEveryToolTheModelGetsIsAValidMessagesAPITool(t *testing.T) {
	tools := modelTools()
	if len(tools) != len(modelToolNames) {
		t.Fatalf("%d tools for %d names", len(tools), len(modelToolNames))
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	json.Unmarshal(raw, &back)
	for _, tl := range back {
		name, _ := tl["name"].(string)
		if !toolNameRE.MatchString(name) {
			t.Errorf("tool name %q", name)
		}
		if d, _ := tl["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("%s: no description", name)
		}
		sch, _ := tl["input_schema"].(map[string]any)
		if sch["type"] != "object" {
			t.Errorf("%s: input_schema.type is %v", name, sch["type"])
		}
		for _, k := range []string{"oneOf", "anyOf", "allOf", "not", "enum"} {
			if _, has := sch[k]; has {
				t.Errorf("%s: top-level %s", name, k)
			}
		}
		var errs []string
		validSchema(name, sch, &errs)
		for _, e := range errs {
			t.Error(e)
		}
	}
}

// requestShapeProblems checks a request body, as the API receives it, against
// the Messages API's structural rules for what the review sends.
func requestShapeProblems(req map[string]any) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	known := map[string]bool{"model": true, "max_tokens": true, "system": true, "messages": true, "tools": true, "tool_choice": true}
	for k := range req {
		if !known[k] {
			add("unknown top-level field %s", k)
		}
	}
	if s, has := req["system"]; has {
		if str, ok := s.(string); !ok || strings.TrimSpace(str) == "" {
			add("system is empty or not a string")
		}
	}
	tools, _ := req["tools"].([]any)
	seen := map[string]bool{}
	for _, tl := range tools {
		tt, _ := tl.(map[string]any)
		name, _ := tt["name"].(string)
		if !toolNameRE.MatchString(name) || seen[name] {
			add("tool name %q is invalid or repeated", name)
		}
		seen[name] = true
		if d, _ := tt["description"].(string); strings.TrimSpace(d) == "" {
			add("tool %s has no description", name)
		}
		sch, _ := tt["input_schema"].(map[string]any)
		if sch["type"] != "object" {
			add("tool %s: input_schema.type is not object", name)
		}
		var errs []string
		validSchema(name, sch, &errs)
		out = append(out, errs...)
	}
	msgs, _ := req["messages"].([]any)
	var wantResults []string
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		switch c := mm["content"].(type) {
		case string:
			if strings.TrimSpace(c) == "" {
				add("message %d: empty content", i)
			}
		case []any:
			if len(c) == 0 {
				add("message %d: no content blocks", i)
			}
			var got []string
			var uses []string
			for j, b := range c {
				bb, _ := b.(map[string]any)
				switch bb["type"] {
				case "text":
					if s, ok := bb["text"].(string); !ok || strings.TrimSpace(s) == "" {
						add("message %d block %d: text block without non-whitespace text", i, j)
					}
				case "tool_use":
					if id, _ := bb["id"].(string); id == "" {
						add("message %d block %d: tool_use without id", i, j)
					} else {
						uses = append(uses, id)
					}
					if n, _ := bb["name"].(string); n == "" {
						add("message %d block %d: tool_use without a name", i, j)
					}
					if in, ok := bb["input"].(map[string]any); !ok || in == nil {
						add("message %d block %d: tool_use without an input object", i, j)
					}
					if _, has := bb["text"]; has {
						add("message %d block %d: tool_use carries text", i, j)
					}
				case "tool_result":
					id, _ := bb["tool_use_id"].(string)
					got = append(got, id)
					if s, ok := bb["content"].(string); !ok || s == "" {
						add("message %d block %d: tool_result without string content", i, j)
					}
				default:
					add("message %d block %d: block type %v", i, j, bb["type"])
				}
			}
			if i > 0 && len(wantResults) > 0 {
				if fmt.Sprint(got) != fmt.Sprint(wantResults) {
					add("message %d: tool results %v for tool uses %v", i, got, wantResults)
				}
			}
			wantResults = uses
		default:
			add("message %d: content is neither a string nor blocks", i)
		}
	}
	return out
}

func TestTheShapeCheckCatchesWhatTheAPIRefuses(t *testing.T) {
	bad := map[string]any{"model": "m", "max_tokens": 1.0, "messages": []any{
		map[string]any{"role": "user", "content": "x"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text"}, map[string]any{"type": "tool_use", "id": "t1", "name": "x"}}},
	}}
	if got := requestShapeProblems(bad); len(got) < 2 {
		t.Errorf("not caught: %v", got)
	}
}
