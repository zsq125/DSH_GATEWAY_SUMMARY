package upstream

import (
	"encoding/json"
	"strings"
)

// PrepareBody OpenAI 请求体 → Trae SOLO llm_utils_chat 请求体。
//
// 规则：
//  1. messages[].content：字符串 → [{"type":"text","text":...}]；已是数组 → 透传
//  2. assistant.tool_calls[]：OpenAI 的 function{name,arguments} → 上游 function_call
//  3. stream 强制 true（非流式由网关本地聚合）
//  4. model → 同时写 config_name 与 model 字段
//  5. function 固定 "solo_work_lite"
//  6. tools[].function.parameters：object → JSON 字符串（上游 struct 声明为 string）
//  7. tool_choice：对象形态归一化为字符串（none 则删 tools）
func PrepareBody(src []byte, defaultModel string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src // 无法解析原样透传，交由上游报错
	}
	obj["stream"] = true
	obj["function"] = Function

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)
			if role == "assistant" {
				normalizeAssistantToolCalls(m)
			}
			if !present || content == nil {
				continue
			}
			if c, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": c}}
			}
		}
	}

	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultModel
		if model == "" {
			model = DefaultConfigName
		}
	}
	obj["config_name"] = model
	obj["model"] = model

	normalizeToolChoice(obj)
	normalizeTools(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeAssistantToolCalls 把 assistant 的 tool_calls.function 转成上游
// function_call 形态，并剔除无 name 的条目。
func normalizeAssistantToolCalls(m map[string]any) {
	tcs, ok := m["tool_calls"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tcs))
	for _, tci := range tcs {
		tc, ok := tci.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			tc["function_call"] = fn
			delete(tc, "function")
		}
		if fc, ok := tc["function_call"].(map[string]any); ok {
			name, _ := fc["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue // 上游要求 name 必填
			}
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		delete(m, "tool_calls")
		return
	}
	m["tool_calls"] = kept
}

// normalizeToolChoice 对象形态 → 字符串（"none" 一并删 tools）。
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 转换 tools 条目：parameters object → JSON 字符串，非法条目剔除。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if pm, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(pm); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
