package service

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const maxCyberPolicyExcerptRunes = 20000

// cyberPolicyInputExcerpt is used only after an upstream cyber_policy event.
// ponytail: request-carried context only; previous_response_id history is not fetched.
func cyberPolicyInputExcerpt(protocol string, body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	var input gjson.Result
	switch protocol {
	case ContentModerationProtocolOpenAIResponses:
		input = gjson.GetBytes(body, "input")
	case ContentModerationProtocolOpenAIChat, ContentModerationProtocolAnthropicMessages:
		input = gjson.GetBytes(body, "messages")
	default:
		return ""
	}
	items := input.Array()
	if input.Exists() && !input.IsArray() {
		items = []gjson.Result{input}
	}
	header := "[上游 cyber_policy 标记，非人工违规结论；仅本次请求携带的近期片段，非完整会话。系统/开发者提示、图片及推理数据不保存。]\n"
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		header += "[请求使用 previous_response_id，未获取上游保存的历史。]\n"
	}
	remaining := maxCyberPolicyExcerptRunes - utf8.RuneCountInString(header) - 80
	var chunks []string
	truncated := len(items) > 24
	for i := len(items) - 1; i >= max(0, len(items)-24); i-- {
		var parts []string
		nodes := 0
		partRunes := 0
		var collect func(gjson.Result, int)
		add := func(v gjson.Result) {
			if !v.Exists() || v.Type == gjson.Null {
				return
			}
			if partRunes >= 4000 {
				truncated = true
				return
			}
			s := v.String()
			// Never cut raw secrets before redaction; omit oversized fields entirely.
			if len(s) > 256*1024 {
				parts = append(parts, "[字段过长，未保存]")
				truncated = true
				return
			}
			if s = redactContentModerationSecrets(s); s != "" {
				if utf8.RuneCountInString(s) > 4000-partRunes {
					s = trimRunes(s, 4000-partRunes)
					truncated = true
				}
				parts = append(parts, s)
				partRunes += utf8.RuneCountInString(s)
			}
		}
		collect = func(v gjson.Result, depth int) {
			if !v.Exists() || v.Type == gjson.Null {
				return
			}
			nodes++
			if depth > 8 || nodes > 256 {
				truncated = true
				return
			}
			if v.IsArray() {
				v.ForEach(func(_, child gjson.Result) bool {
					collect(child, depth+1)
					return nodes <= 256
				})
				return
			}
			if v.Type == gjson.String {
				add(v)
				return
			}
			role := strings.ToLower(strings.TrimSpace(v.Get("role").String()))
			if role == "system" || role == "developer" {
				return
			}
			switch v.Get("type").String() {
			case "", "message", "text", "input_text", "output_text":
				add(v.Get("text"))
				collect(v.Get("content"), depth+1)
				collect(v.Get("tool_calls"), depth+1)
				if fn := v.Get("function_call"); fn.Exists() {
					parts = append(parts, "[function_call]")
					add(fn.Get("name"))
					add(fn.Get("arguments"))
				}
			case "function", "function_call":
				parts = append(parts, "[function_call]")
				fn := v
				if v.Get("function").Exists() {
					fn = v.Get("function")
				}
				add(fn.Get("name"))
				add(fn.Get("arguments"))
			case "custom_tool_call", "tool_use":
				parts = append(parts, "[tool_call]")
				add(v.Get("name"))
				add(v.Get("input"))
			case "function_call_output", "custom_tool_call_output", "tool_result":
				parts = append(parts, "[tool_result]")
				collect(v.Get("output"), depth+1)
				collect(v.Get("content"), depth+1)
			}
		}
		collect(items[i], 0)
		if len(parts) == 0 {
			continue
		}
		role := items[i].Get("role").String()
		switch role {
		case "user", "assistant", "tool", "function":
		default:
			role = "input"
		}
		chunk := fmt.Sprintf("[%d %s]\n%s", i+1, role, strings.Join(parts, "\n"))
		limit := min(4000, remaining)
		if utf8.RuneCountInString(chunk) > limit {
			chunk = trimRunes(chunk, limit-20) + "\n[本项已截断]"
			truncated = true
		}
		chunks = append(chunks, chunk)
		remaining -= utf8.RuneCountInString(chunk) + 2
		if remaining < 100 {
			truncated = truncated || i > 0
			break
		}
	}
	if len(chunks) == 0 {
		return header + "[请求中无可保存的文本片段]"
	}
	slices.Reverse(chunks)
	if truncated {
		header += "[已截断：最多最近24项，每项4000字，总计20000字；大字段不保存。]\n"
	}
	return trimRunes(header+strings.Join(chunks, "\n\n"), maxCyberPolicyExcerptRunes)
}
