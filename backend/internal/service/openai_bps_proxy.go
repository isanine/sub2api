package service

// BPS 代理：把 Team 账号的 /responses 流量转发到 ChatGPT for Excel 加载项后端
// （bps.openai.com/basispoints）。协议翻译移植自社区项目 kokojacket/openai-proxy
// 与 Kaixxrua/excel-codex-bridge（MIT/Unlicense）：
//   - 请求侧：客户端工具目录写进 developer 提示词，经原生 run_officejs 传输；
//     function_call / function_call_output 历史在两种形态间往返（调用记忆）。
//   - 响应侧：SSE 流改写器把上游 run_officejs 调用还原为客户端 function_call
//     事件，隐藏宿主工具细节。
//   - 账号状态：403 → 本账号 BPS 封禁 1 小时 + 优先级 +2（一次）；到期后下次
//     请求先探测，成功则解封并还原优先级。
//
// v1 范围裁剪：不含图片附件管道、code-mode 桥、office/no-call 自动续跳
// （单跳直通，格式坏由客户端重试承担）。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	bpsResponsesURL    = "https://bps.openai.com/basispoints/api/responses"
	bpsTransportTool   = "run_officejs"
	bpsDefaultModel    = "gpt-5.6-sol"
	bpsOfficeOK        = `{"status":"ok"}`
	bpsDemotedExtraKey = "bps_priority_demoted"
)

func bpsIsTransportName(name string) bool {
	return name == bpsTransportTool || name == "functions."+bpsTransportTool
}

var bpsModelAliases = map[string]string{
	"gpt-6-sol":           "gpt-5.6-sol",
	"gpt-6-terra":         "gpt-5.6-terra",
	"gpt-6-luna":          "gpt-5.6-luna",
	"gpt-5.6-sol-excel":   "gpt-5.6-sol",
	"gpt-5.6-luna-excel":  "gpt-5.6-luna",
	"gpt-5.6-terra-excel": "gpt-5.6-terra",
	"gpt-6-astra-excel":   "gpt-6-astra",
}

var bpsEffortAliases = map[string]string{
	"x-high": "xhigh", "extra-high": "xhigh", "extra_high": "xhigh",
	"max": "xhigh", "persistent": "xhigh", "minimal": "low", "none": "low",
}

func bpsNormalizeEffort(v string) string {
	low := strings.ToLower(strings.TrimSpace(v))
	if alias, ok := bpsEffortAliases[low]; ok {
		low = alias
	}
	switch low {
	case "low", "medium", "high", "xhigh", "ultra":
		return low
	}
	return "medium"
}

func bpsUpstreamModel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return bpsDefaultModel
	}
	if alias, ok := bpsModelAliases[v]; ok {
		return alias
	}
	return v
}

// ————— 基础序列化工具（与参考实现的 _sha/_uuid_for/_dumps 对齐） —————

func bpsDumps(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func bpsSha256JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var bpsUUIDNamespace = uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8") // NAMESPACE_URL

func bpsUUIDFor(text string) string {
	return uuid.NewSHA1(bpsUUIDNamespace, []byte(text)).String()
}

// ————— 客户端工具目录 —————

type bpsClientTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Format      map[string]any `json:"format,omitempty"`
}

// bpsDeclaredClientTools 读取客户端声明的工具目录（顶层 tools + Responses Lite
// additional_tools）。origin: none / top_level / additional_tools / mixed。
func bpsDeclaredClientTools(source map[string]any) (tools []bpsClientTool, origin string) {
	var groups []any
	top := false
	if raw, ok := source["tools"]; ok {
		top = true
		groups = append(groups, raw)
	}
	lite := false
	if input, ok := source["input"].([]any); ok {
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "additional_tools" {
				continue
			}
			lite = true
			groups = append(groups, m["tools"])
		}
	}
	merged := map[string]bpsClientTool{}
	names := []string{}
	for _, group := range groups {
		collected := []bpsClientTool{}
		bpsIterClientTools(group, &collected, 0)
		for _, tool := range collected {
			if prev, ok := merged[tool.Name]; ok && bpsDumps(prev) != bpsDumps(tool) {
				continue // 同名冲突保留先到的，容忍合并
			}
			if _, exists := merged[tool.Name]; !exists {
				names = append(names, tool.Name)
			}
			merged[tool.Name] = tool
		}
	}
	sort.Strings(names)
	for _, name := range names {
		tools = append(tools, merged[name])
	}
	switch {
	case top && lite:
		origin = "mixed"
	case lite:
		origin = "additional_tools"
	case top:
		origin = "top_level"
	default:
		origin = "none"
	}
	return tools, origin
}

func bpsIterClientTools(tools any, collected *[]bpsClientTool, depth int) {
	list, ok := tools.([]any)
	if !ok || depth > 4 {
		return
	}
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if nested, ok := tool["tools"].([]any); ok {
			bpsIterClientTools(nested, collected, depth+1)
			if tool["type"] == "namespace" {
				continue
			}
		}
		toolType := "function"
		if t, ok := tool["type"].(string); ok && t != "" {
			toolType = t
		}
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		parameters, _ := tool["parameters"].(map[string]any)
		if nested, ok := tool["function"].(map[string]any); ok {
			if v, ok := nested["name"].(string); ok && v != "" {
				name = v
			}
			if v, ok := nested["description"].(string); ok && v != "" {
				description = v
			}
			if v, ok := nested["parameters"].(map[string]any); ok {
				parameters = v
			}
		}
		if name == "" || bpsIsTransportName(name) {
			continue
		}
		if parameters == nil {
			parameters = map[string]any{}
		}
		entry := bpsClientTool{Type: toolType, Name: name, Description: description, Parameters: parameters}
		if format, ok := tool["format"].(map[string]any); ok {
			entry.Format = format
		}
		*collected = append(*collected, entry)
	}
}

func bpsDescribeSchema(schema any, indent int) []string {
	m, ok := schema.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	properties, ok := m["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return nil
	}
	required := map[string]bool{}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pad := strings.Repeat("  ", indent)
	lines := []string{}
	for _, key := range keys {
		spec, ok := properties[key].(map[string]any)
		if !ok {
			continue
		}
		kind := "value"
		if t, ok := spec["type"].(string); ok && t != "" {
			kind = t
		} else if _, isList := spec["type"].([]any); isList {
			parts := []string{}
			if ts, ok := spec["type"].([]any); ok {
				for _, t := range ts {
					parts = append(parts, fmt.Sprint(t))
				}
			}
			kind = strings.Join(parts, "/")
		}
		flag := "optional"
		if required[key] {
			flag = "required"
		}
		suffix := ""
		if d, ok := spec["description"].(string); ok && d != "" {
			suffix = ": " + d
		}
		lines = append(lines, fmt.Sprintf("%s- %s (%s, %s)%s", pad, key, kind, flag, suffix))
		if enum, ok := spec["enum"].([]any); ok {
			vals := []string{}
			for _, v := range enum {
				vals = append(vals, fmt.Sprintf("%q", v))
			}
			lines = append(lines, pad+"  Allowed values: "+strings.Join(vals, ", "))
		}
		nested := spec
		if kind == "array" {
			nested, _ = spec["items"].(map[string]any)
		}
		if nested != nil {
			if _, has := nested["properties"]; has {
				lines = append(lines, bpsDescribeSchema(nested, indent+1)...)
			}
		}
	}
	return lines
}

func bpsTransportShapes(tools []bpsClientTool) []string {
	hasFunction, hasCustom := false, false
	for _, tool := range tools {
		if tool.Type != "custom" {
			hasFunction = true
		} else {
			hasCustom = true
		}
	}
	lines := []string{}
	if hasFunction {
		lines = append(lines, `Function tools use {"tool":"TOOL_NAME","args":{...}}; args is a JSON object.`)
	}
	if hasCustom {
		lines = append(lines, `Custom tools use {"tool":"TOOL_NAME","input":"raw text"}; input is a required string.`)
	}
	return lines
}

const bpsExecutionGuidance = "Use declared client tools when the user's task needs execution; answer directly when it does not. " +
	"Follow the client's declarations and existing authorization. Report observed permission limits, " +
	"refusals and tool errors accurately; do not invent capabilities or bypass these limits. " +
	"Preserve completed work; do not repeat operations already executed in the history. " +
	"Only claim an action ran when its tool result confirms it. For file creation or packaging, " +
	"verify the resulting file and relevant contents or archive integrity before reporting completion, " +
	"and give the verified output location. Distinguish completed work from unverified work."

func bpsProtocolInstructions(tools []bpsClientTool) string {
	if len(tools) == 0 {
		return "This is an external Responses client session. No client tools are enabled for this request. " +
			"Answer in assistant text using the available context. The host Office tools are outside " +
			"this session and must not be called. Report only actions supported by existing tool results."
	}
	lines := []string{
		"You are assisting an external Responses client. Its available tools are listed below.",
		"Route client tool calls through run_officejs; its code field carries data to the client.",
		"Give each independent client call its own wrapper. code contains JSON, not Office.js.",
		"code is a JSON string containing one envelope. Choose its shape by the client tool type:",
	}
	lines = append(lines, bpsTransportShapes(tools)...)
	lines = append(lines,
		"The outer run_officejs arguments must also include summary (short text), destructive (boolean), and references (array of strings).",
		"Do not set the inner tool name to run_officejs. Do not nest another envelope inside code.",
		"Treat each returned result as the named client tool's output and continue from that evidence.",
		"Only run_officejs serves as the host transport; other host Office tools are outside this session.",
		bpsExecutionGuidance,
		"Available client tools:",
	)
	for _, tool := range tools {
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			description = "No description."
		}
		lines = append(lines, fmt.Sprintf("\nTool `%s`: %s", tool.Name, description))
		if tool.Type == "custom" {
			lines = append(lines, fmt.Sprintf(`Custom tool. Its code object is {"tool":"%s","input":"raw text"}.`, tool.Name))
			lines = append(lines, "Input: required string containing the raw tool input, not an args object.")
			if form := tool.Format; form != nil {
				if syntax, ok := form["syntax"].(string); ok && syntax != "" {
					lines = append(lines, "Input grammar syntax: "+syntax)
				}
				if def, ok := form["definition"].(string); ok && def != "" {
					lines = append(lines, def)
				}
			}
			continue
		}
		params := bpsDescribeSchema(tool.Parameters, 0)
		if len(params) > 0 {
			lines = append(lines, "Arguments:")
			lines = append(lines, params...)
		} else {
			lines = append(lines, "Arguments: a JSON object following the tool description.")
		}
	}
	lines = append(lines, "\nRemember: each client call needs its own wrapper and the envelope shape for that tool type.")
	lines = append(lines, `For JSON strings, escape backslashes and quotes. Do not use arguments.patch.`)
	return strings.Join(lines, "\n")
}

// ————— 调用记忆（每会话隔离、有界） —————

type bpsCallMemory struct {
	mu         sync.Mutex
	items      map[string]map[string]any // callID → native run_officejs item
	order      []string
	tools      map[string][]bpsClientTool // conversation → catalog
	iterations map[string]int             // turnID → iteration
	callTurns  map[string]bpsTurnState
}

type bpsTurnState struct {
	TurnID    string
	Iteration int
}

func newBPSCallMemory() *bpsCallMemory {
	return &bpsCallMemory{
		items:      map[string]map[string]any{},
		tools:      map[string][]bpsClientTool{},
		iterations: map[string]int{},
		callTurns:  map[string]bpsTurnState{},
	}
}

const bpsMemoryLimit = 512

func (m *bpsCallMemory) remember(scope, callID string, item map[string]any, turnID string, iteration int) {
	if callID == "" || item == nil {
		return
	}
	key := scope + ":" + callID
	stored := bpsDeepCopyMap(item)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.items[key]; !exists {
		m.order = append(m.order, key)
	}
	m.items[key] = stored
	if turnID != "" && iteration > 0 {
		m.callTurns[key] = bpsTurnState{TurnID: turnID, Iteration: iteration}
	}
	for len(m.order) > bpsMemoryLimit {
		drop := m.order[0]
		m.order = m.order[1:]
		delete(m.items, drop)
		delete(m.callTurns, drop)
	}
}

func (m *bpsCallMemory) recall(scope, callID string) map[string]any {
	if callID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if item, ok := m.items[scope+":"+callID]; ok {
		return bpsDeepCopyMap(item)
	}
	return nil
}

func (m *bpsCallMemory) bindTools(scope string, source map[string]any) []bpsClientTool {
	parsed, origin := bpsDeclaredClientTools(source)
	conversation := scope + ":" + bpsConversationIdentity(source)
	choice := source["tool_choice"]
	m.mu.Lock()
	if len(parsed) > 0 || origin == "additional_tools" || origin == "mixed" {
		m.tools[conversation] = parsed
	}
	stored := append([]bpsClientTool(nil), m.tools[conversation]...)
	for len(m.tools) > 32 {
		for key := range m.tools {
			delete(m.tools, key)
			break
		}
	}
	m.mu.Unlock()
	if choice == "none" {
		return nil
	}
	if choiceMap, ok := choice.(map[string]any); ok {
		name, _ := choiceMap["name"].(string)
		if name == "" {
			if fn, ok := choiceMap["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		filtered := []bpsClientTool{}
		for _, tool := range stored {
			if tool.Name == name {
				filtered = append(filtered, tool)
			}
		}
		return filtered
	}
	return stored
}

func (m *bpsCallMemory) advanceTurn(turnID string, minimum int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	value := minimum
	if v := m.iterations[turnID] + 1; v > value {
		value = v
	}
	m.iterations[turnID] = value
	for len(m.iterations) > bpsMemoryLimit {
		for key := range m.iterations {
			delete(m.iterations, key)
			break
		}
	}
	return value
}

func bpsDeepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ————— 会话身份 —————

func bpsMessageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

func bpsPartText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		chunks := []string{}
		for _, part := range v {
			switch p := part.(type) {
			case string:
				chunks = append(chunks, p)
			case map[string]any:
				if text, ok := p["text"].(string); ok {
					chunks = append(chunks, text)
				}
			}
		}
		return strings.Join(chunks, "")
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
	}
	return ""
}

func bpsIdentityItems(rawInput any) []map[string]any {
	result := []map[string]any{}
	list, ok := rawInput.([]any)
	if !ok {
		if s, isStr := rawInput.(string); isStr {
			return []map[string]any{bpsMessageItem("user", s)}
		}
		return result
	}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "message" || (item["type"] == nil && item["role"] != nil) {
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			result = append(result, map[string]any{
				"type": "message", "role": role,
				"content": bpsNormalizeContent(role, item["content"]),
			})
		} else {
			result = append(result, bpsDeepCopyMap(item))
		}
	}
	return result
}

func bpsConversationIdentity(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "session_id"} {
		if v, ok := source[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if metadata, ok := source["client_metadata"].(map[string]any); ok {
		for _, key := range []string{"session_id", "sessionId"} {
			if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	items := bpsIdentityItems(source["input"])
	for i, item := range items {
		if item["role"] == "user" {
			return bpsSha256JSON(items[:i+1])
		}
	}
	if len(items) > 0 {
		return bpsSha256JSON(items[:1])
	}
	return "anonymous"
}

func bpsTurnStateKey(rawInput any) (string, string) {
	items := bpsIdentityItems(rawInput)
	if len(items) == 0 {
		return "anonymous", "1"
	}
	lastUser := -1
	for i, item := range items {
		if item["role"] == "user" {
			lastUser = i
		}
	}
	prefix := items
	if lastUser >= 0 {
		prefix = items[:lastUser+1]
	} else {
		prefix = items[:1]
	}
	rounds := 0
	inOutputs := false
	for _, item := range items[lastUser+1:] {
		t, _ := item["type"].(string)
		isOutput := t == "function_call_output" || t == "custom_tool_call_output"
		if isOutput && !inOutputs {
			rounds++
		}
		inOutputs = isOutput
	}
	return bpsSha256JSON(prefix), fmt.Sprint(rounds + 1)
}

// ————— 请求翻译 —————

func bpsNormalizeContent(role string, content any) []any {
	switch v := content.(type) {
	case string:
		textType := "input_text"
		if role == "assistant" {
			textType = "output_text"
		}
		return []any{map[string]any{"type": textType, "text": v}}
	case []any:
		out := []any{}
		for _, part := range v {
			switch p := part.(type) {
			case string:
				out = append(out, map[string]any{"type": "input_text", "text": p})
			case map[string]any:
				if p["type"] == "input_image" || p["type"] == "image" {
					// v1：图片不入 BPS 请求，以占位文本代替。
					out = append(out, map[string]any{"type": "input_text", "text": "[image omitted]"})
					continue
				}
				out = append(out, bpsDeepCopyMap(p))
			}
		}
		return out
	case nil:
		return []any{}
	default:
		return []any{map[string]any{"type": "input_text", "text": bpsPartText(content)}}
	}
}

func bpsStripPrivate(item map[string]any) map[string]any {
	out := bpsDeepCopyMap(item)
	delete(out, "bpsexec") // 内部字段不外发
	return out
}

func bpsFCItemID(itemID any) any {
	s, ok := itemID.(string)
	if !ok || s == "" || strings.HasPrefix(s, "fc") {
		return itemID
	}
	for _, prefix := range []string{"ctco_", "ctc_"} {
		if strings.HasPrefix(s, prefix) {
			return "fc_" + s[len(prefix):]
		}
	}
	return "fc_" + s
}

func bpsToolOutputItem(item map[string]any, native map[string]any) map[string]any {
	var output any
	switch v := item["output"].(type) {
	case []any:
		output = v
	case string:
		if strings.TrimSpace(v) == "" {
			output = "(tool call succeeded with no output)"
		} else {
			output = v
		}
	case nil:
		output = "(tool call succeeded with no output)"
	default:
		output = bpsDumps(v)
	}
	custom := native != nil && native["type"] == "custom_tool_call"
	kind := "function_call_output"
	if custom {
		kind = "custom_tool_call_output"
	}
	rewritten := map[string]any{"type": kind, "call_id": item["call_id"], "output": output}
	// 空 id 不写入：拼出来的 "fc_" 会被上游 400 拒绝（Invalid input id）。
	if id, ok := item["id"].(string); ok && strings.TrimSpace(id) != "" {
		if custom {
			rewritten["id"] = id
		} else if mapped, isStr := bpsFCItemID(id).(string); isStr && mapped != "" {
			rewritten["id"] = mapped
		}
	}
	return rewritten
}

// bpsTranslateInput 把客户端 input 历史翻成 BPS 形态：function_call/output 经
// 调用记忆还原为 run_officejs 往返形态，其余消息规范化。
func bpsTranslateInput(rawInput any, mem *bpsCallMemory, scope string) []map[string]any {
	if s, ok := rawInput.(string); ok {
		return []map[string]any{bpsMessageItem("user", s)}
	}
	list, ok := rawInput.([]any)
	if !ok {
		return nil
	}
	result := []map[string]any{}
	seen := map[string]bool{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item = bpsStripPrivate(item)
		kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["type"])))
		switch kind {
		case "additional_tools":
			continue
		case "function_call", "custom_tool_call":
			callID, _ := item["call_id"].(string)
			if seen[callID] {
				continue
			}
			remembered := mem.recall(scope, callID)
			var native map[string]any
			if remembered != nil {
				native = remembered
			} else if name, _ := item["name"].(string); bpsIsTransportName(name) || name == "update_plan" {
				native = item
			} else {
				native = bpsFallbackTransport(item)
			}
			result = append(result, bpsDeepCopyMap(native))
			seen[callID] = true
		case "function_call_output", "custom_tool_call_output":
			callID, _ := item["call_id"].(string)
			native := mem.recall(scope, callID)
			if !seen[callID] && native != nil {
				result = append(result, native)
				seen[callID] = true
			}
			output := bpsToolOutputItem(item, native)
			if native != nil && native["name"] == "update_plan" {
				output["output"] = bpsOfficeOK
			}
			result = append(result, output)
		case "reasoning":
			if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
				result = append(result, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
			}
		case "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			result = append(result, map[string]any{
				"type": "message", "role": role,
				"content": bpsNormalizeContent(role, item["content"]),
			})
		case "item_reference":
			// skip
		default:
			if item["role"] != nil && kind == "" {
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				result = append(result, map[string]any{
					"type": "message", "role": role,
					"content": bpsNormalizeContent(role, item["content"]),
				})
			} else if kind != "" {
				result = append(result, item)
			}
		}
	}
	return result
}

func bpsIsCompaction(source map[string]any) bool {
	input, ok := source["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}
	last, ok := input[len(input)-1].(map[string]any)
	return ok && last["type"] == "compaction_trigger"
}

// bpsRequestedEffort 提取请求的推理力度（含 configuration_update）。
func bpsRequestedEffort(source map[string]any) string {
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			return effort
		}
	}
	for _, key := range []string{"reasoning_effort", "model_reasoning_effort"} {
		if v, ok := source[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	if input, ok := source["input"].([]any); ok {
		found := ""
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok || item["type"] != "configuration_update" {
				continue
			}
			if v, ok := item["reasoning_effort"].(string); ok {
				found = v
			} else if v, ok := item["effort"].(string); ok {
				found = v
			} else if r, ok := item["reasoning"].(map[string]any); ok {
				if v, ok := r["effort"].(string); ok {
					found = v
				}
			}
		}
		if found != "" {
			return found
		}
	}
	return "medium"
}

// bpsPrepareBody 构造 BPS 上游请求体。
func bpsPrepareBody(source map[string]any, mem *bpsCallMemory, scope string, requestedModel string) (map[string]any, string, int) {
	tools := mem.bindTools(scope, source)
	history := bpsTranslateInput(source["input"], mem, scope)
	conversation := bpsConversationIdentity(source)
	fingerprint, iterationStr := bpsTurnStateKey(source["input"])
	turnID := bpsUUIDFor("bps-proxy/" + conversation + "/turn/" + fingerprint)
	minimum := 1
	if n := parseIntSafe(iterationStr); n > minimum {
		minimum = n
	}
	iteration := mem.advanceTurn(scope+":"+turnID, minimum)

	prologue := []map[string]any{}
	if instructions, ok := source["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		prologue = append(prologue, bpsMessageItem("developer", strings.TrimSpace(instructions)))
	}
	if !bpsIsCompaction(source) {
		prompt := bpsProtocolInstructions(tools)
		if source["tool_choice"] == "required" || source["tool_choice"] != nil && isMap(source["tool_choice"]) {
			if len(tools) > 0 {
				prompt += "\nUse at least one of the available client tools before answering."
			}
		}
		if parallel, ok := source["parallel_tool_calls"].(bool); ok && !parallel {
			prompt += "\nIssue only one client tool call in each response."
		}
		prologue = append(prologue, bpsMessageItem("developer", prompt))
	}

	input := make([]any, 0, len(prologue)+len(history))
	for _, item := range prologue {
		input = append(input, item)
	}
	for _, item := range history {
		input = append(input, item)
	}

	model := bpsUpstreamModel(modelString(source["model"]))
	body := map[string]any{
		"model":            model,
		"model_selection":  "explicit",
		"stream":           true,
		"store":            false,
		"input":            input,
		"reasoning_effort": bpsNormalizeEffort(bpsRequestedEffort(source)),
		"context_management": []any{
			map[string]any{"type": "compaction", "compact_threshold": 200000},
		},
		"metadata": map[string]any{
			"agent_iteration": fmt.Sprint(iteration),
			"task_id":         bpsUUIDFor("bps-proxy/" + conversation),
			"turn_id":         turnID,
		},
	}
	if cacheKey, ok := source["prompt_cache_key"].(string); ok && strings.TrimSpace(cacheKey) != "" {
		body["prompt_cache_key"] = strings.TrimSpace(cacheKey)
	}
	if _, hasCustom := source["context_management"]; hasCustom {
		body["context_management"] = source["context_management"]
	}
	return body, turnID, iteration
}

func parseIntSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func modelString(v any) string {
	s, _ := v.(string)
	return s
}

// ————— 传输信封解码（宽松 JSON） —————

var bpsJSONEscapes = map[rune]bool{'"': true, '\\': true, '/': true, 'b': true, 'f': true, 'n': true, 'r': true, 't': true, 'u': true}

func bpsRelaxJSONText(text string) string {
	var out strings.Builder
	inString := false
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if inString {
			switch {
			case c == '\\':
				var nxt rune
				if i+1 < len(runes) {
					nxt = runes[i+1]
				}
				if bpsJSONEscapes[nxt] {
					out.WriteRune(c)
					out.WriteRune(nxt)
					i++
					continue
				}
				out.WriteString("\\\\")
				continue
			case c == '"':
				inString = false
				out.WriteRune(c)
				continue
			case c == '\n':
				out.WriteString("\\n")
			case c == '\r':
				out.WriteString("\\r")
			case c == '\t':
				out.WriteString("\\t")
			case c < 32:
				out.WriteString(fmt.Sprintf("\\u%04x", c))
			default:
				out.WriteRune(c)
			}
			continue
		}
		if c == '"' {
			inString = true
		}
		out.WriteRune(c)
	}
	return out.String()
}

func bpsLoadsLenient(text string) (any, bool) {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err == nil {
		return v, true
	}
	relaxed := bpsRelaxJSONText(text)
	if relaxed == text {
		return nil, false
	}
	if err := json.Unmarshal([]byte(relaxed), &v); err == nil {
		return v, true
	}
	return nil, false
}

func bpsDecodeTransportCode(code any, depth int) map[string]any {
	if depth > 4 {
		return nil
	}
	var value any
	switch v := code.(type) {
	case map[string]any:
		value = v
	case string:
		loaded, ok := bpsLoadsLenient(v)
		if !ok {
			return nil
		}
		value = loaded
	default:
		return nil
	}
	if s, ok := value.(string); ok {
		return bpsDecodeTransportCode(s, depth+1)
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	name, _ := m["tool"].(string)
	if name == "" {
		name, _ = m["name"].(string)
	}
	if bpsIsTransportName(name) {
		var nested any
		if n, ok := m["arguments"]; ok {
			nested = n
		} else if n, ok := m["args"]; ok {
			nested = n
		}
		if s, ok := nested.(string); ok {
			if loaded, ok := bpsLoadsLenient(s); ok {
				nested = loaded
			}
		}
		if nm, ok := nested.(map[string]any); ok {
			return bpsDecodeTransportCode(nm["code"], depth+1)
		}
		return nil
	}
	return m
}

func bpsEnvelopeCall(envelope map[string]any, allowed map[string]bool) (string, any, bool) {
	name, _ := envelope["tool"].(string)
	if name == "" {
		name, _ = envelope["name"].(string)
	}
	if name == "" || !allowed[name] || bpsIsTransportName(name) {
		return "", nil, false
	}
	var arguments any
	if args, ok := envelope["args"]; ok {
		arguments = args
	} else if args, ok := envelope["arguments"]; ok {
		arguments = args
	} else if input, ok := envelope["input"]; ok {
		arguments = map[string]any{"input": input}
	} else {
		arguments = map[string]any{}
	}
	if s, ok := arguments.(string); ok {
		if loaded, ok := bpsLoadsLenient(s); ok {
			if m, isMap := loaded.(map[string]any); isMap {
				arguments = m
			} else {
				return name, s, true
			}
		}
	}
	if _, ok := arguments.(map[string]any); !ok {
		return "", nil, false
	}
	return name, arguments, true
}

// ————— 传输翻译（上游 → 客户端） —————

func bpsTranslateTransport(item map[string]any, allowed map[string]bool, custom map[string]bool) (map[string]any, string) {
	if item["type"] == "custom_tool_call" {
		name, _ := item["name"].(string)
		if !allowed[name] || !custom[name] {
			return nil, "unknown_tool"
		}
		if callID, _ := item["call_id"].(string); callID == "" {
			return nil, "missing_call_id"
		}
		if _, ok := item["input"].(string); !ok {
			return nil, "bad_arguments"
		}
		return bpsDeepCopyMap(item), ""
	}
	if item["type"] != "function_call" {
		return nil, "not_function"
	}
	name, _ := item["name"].(string)
	var arguments map[string]any
	switch raw := item["arguments"].(type) {
	case string:
		loaded, ok := bpsLoadsLenient(raw)
		if !ok {
			return nil, "outer_json"
		}
		m, isMap := loaded.(map[string]any)
		if !isMap {
			return nil, "outer_json"
		}
		arguments = m
	case map[string]any:
		arguments = raw
	default:
		return nil, "outer_json"
	}
	if callID, _ := item["call_id"].(string); callID == "" {
		return nil, "missing_call_id"
	}
	var envelope map[string]any
	if name == "update_plan" && allowed[name] {
		plan, _ := arguments["plan"].([]any)
		entries := []any{}
		for _, raw := range plan {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			step, _ := entry["step"].(string)
			if step == "" {
				step, _ = entry["description"].(string)
			}
			status, _ := entry["status"].(string)
			entries = append(entries, map[string]any{"step": step, "status": status})
		}
		explanation, _ := arguments["explanation"].(string)
		if explanation == "" {
			explanation, _ = arguments["summary"].(string)
		}
		envelope = map[string]any{"tool": name, "args": map[string]any{"plan": entries, "explanation": explanation}}
	} else {
		if !bpsIsTransportName(name) {
			return nil, "workbook_tool"
		}
		code, hasCode := arguments["code"]
		if !hasCode {
			return nil, "missing_code"
		}
		envelope = bpsDecodeTransportCode(code, 0)
		if envelope == nil {
			return nil, "inner_json"
		}
	}
	envelope = bpsDeepCopyMap(envelope)
	name, _ = envelope["tool"].(string)
	if name == "" {
		name, _ = envelope["name"].(string)
	}
	if strings.HasPrefix(name, "functions.") && allowed[name[len("functions."):]] {
		envelope["tool"] = name[len("functions."):]
		name = name[len("functions."):]
	}
	if name == "" || !allowed[name] || bpsIsTransportName(name) {
		return nil, "unknown_tool"
	}
	decodedName, payload, ok := bpsEnvelopeCall(envelope, allowed)
	if !ok {
		return nil, "bad_arguments"
	}
	if custom[decodedName] {
		var rawInput string
		switch p := payload.(type) {
		case string:
			rawInput = p
		case map[string]any:
			if s, ok := p["input"].(string); ok {
				rawInput = s
			} else if decodedName == "exec" {
				if s, ok := p["code"].(string); ok {
					rawInput = s
				}
			} else {
				return nil, "bad_arguments"
			}
		default:
			return nil, "bad_arguments"
		}
		rewritten := map[string]any{"type": "custom_tool_call", "call_id": item["call_id"], "name": decodedName, "input": rawInput}
		if id, ok := item["id"].(string); ok && id != "" {
			rewritten["id"] = id
		}
		return rewritten, ""
	}
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		return nil, "bad_arguments"
	}
	rewritten := map[string]any{"type": "function_call", "call_id": item["call_id"], "name": decodedName, "arguments": bpsDumps(payloadMap)}
	if id, ok := item["id"].(string); ok && id != "" {
		rewritten["id"] = id
	}
	return rewritten, ""
}

func bpsFallbackTransport(item map[string]any) map[string]any {
	name, _ := item["name"].(string)
	if name == "" {
		name = "tool"
	}
	var arguments map[string]any
	switch raw := item["arguments"].(type) {
	case string:
		if loaded, ok := bpsLoadsLenient(raw); ok {
			if m, isMap := loaded.(map[string]any); isMap {
				arguments = m
			}
		}
	case map[string]any:
		arguments = raw
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID = "call_bps_" + uuid.NewString()
	}
	var code string
	if item["type"] == "custom_tool_call" {
		rawInput, ok := item["input"].(string)
		if !ok {
			rawInput = bpsPartText(item["input"])
		}
		code = bpsDumps(map[string]any{"tool": name, "input": rawInput})
	} else {
		code = bpsDumps(map[string]any{"tool": name, "args": arguments})
	}
	rawID, _ := item["id"].(string)
	fcID, _ := bpsFCItemID(rawID).(string)
	if fcID == "" || !strings.HasPrefix(fcID, "fc") {
		fcID = "fc_" + callID
	}
	return map[string]any{
		"type":      "function_call",
		"id":        fcID,
		"call_id":   callID,
		"name":      bpsTransportTool,
		"arguments": bpsDumps(map[string]any{"summary": "Run client tool " + name, "code": code, "destructive": false, "references": []any{}}),
		"status":    "completed",
	}
}

// ————— SSE 流改写器 —————

type bpsStreamRewriter struct {
	allowed   map[string]bool
	custom    map[string]bool
	mem       *bpsCallMemory
	scope     string
	turnID    string
	iteration int

	pending   map[int]map[string]any
	doneTools map[int]bool
	finished  map[int]bool
	indices   map[int]int
	terminal  bool

	clientCalls []map[string]any
	officeCalls []map[string]any
}

func newBPSStreamRewriter(tools []bpsClientTool, mem *bpsCallMemory, scope, turnID string, iteration int) *bpsStreamRewriter {
	r := &bpsStreamRewriter{
		allowed: map[string]bool{}, custom: map[string]bool{},
		mem: mem, scope: scope, turnID: turnID, iteration: iteration,
		pending: map[int]map[string]any{}, doneTools: map[int]bool{},
		finished: map[int]bool{}, indices: map[int]int{},
	}
	for _, tool := range tools {
		r.allowed[tool.Name] = true
		if tool.Type == "custom" {
			r.custom[tool.Name] = true
		}
	}
	return r
}

type bpsError struct{ msg string }

func (e *bpsError) Error() string { return e.msg }

func (r *bpsStreamRewriter) mapped(index int) int {
	if _, ok := r.indices[index]; !ok {
		r.indices[index] = len(r.indices)
	}
	return r.indices[index]
}

func (r *bpsStreamRewriter) visible(event string, payload map[string]any) (string, map[string]any) {
	copied := bpsDeepCopyMap(payload)
	copied["type"] = event
	if index, ok := copied["output_index"].(float64); ok {
		copied["output_index"] = r.mapped(int(index))
	}
	return event, copied
}

// handle 返回改写后的事件列表；bpsError 表示协议错误（终止流）。
func (r *bpsStreamRewriter) handle(event string, payload map[string]any) ([][2]any, error) {
	if r.terminal {
		return nil, nil
	}
	if event == "response.completed" {
		return r.completed(payload)
	}
	if event == "response.failed" || event == "response.incomplete" {
		r.terminal = true
		response := bpsDeepCopyMap(payload2map(payload["response"]))
		if response == nil {
			response = map[string]any{}
		}
		response["status"] = strings.SplitN(event, ".", 2)[1]
		output := []any{}
		if raw, ok := response["output"].([]any); ok {
			for _, item := range raw {
				if m, isMap := item.(map[string]any); isMap {
					if t, _ := m["type"].(string); t == "function_call" || t == "custom_tool_call" {
						continue
					}
				}
				output = append(output, item)
			}
		}
		response["output"] = output
		payload["response"] = response
		return [][2]any{{event, payload}}, nil
	}
	item, _ := payload["item"].(map[string]any)
	index, hasIndex := payload["output_index"].(float64)
	if (event == "response.output_item.added" || event == "response.output_item.done") && item != nil {
		t, _ := item["type"].(string)
		if t == "function_call" || t == "custom_tool_call" {
			if !hasIndex {
				return nil, &bpsError{"upstream tool item has no output index"}
			}
			r.pending[int(index)] = bpsDeepCopyMap(item)
			if strings.HasSuffix(event, ".done") {
				r.doneTools[int(index)] = true
			}
			return nil, nil
		}
		if strings.HasSuffix(event, ".done") {
			r.finished[int(index)] = true
		}
	}
	if strings.Contains(event, "function_call") || strings.Contains(event, "custom_tool_call") {
		// 原生工具参数绝不外发，改写后在 completed 中统一回放。
		return nil, nil
	}
	name, copied := r.visible(event, payload)
	return [][2]any{{name, copied}}, nil
}

func payload2map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func (r *bpsStreamRewriter) completed(payload map[string]any) ([][2]any, error) {
	response := payload2map(payload["response"])
	if response == nil {
		return nil, &bpsError{"upstream completion has no response"}
	}
	if status, _ := response["status"].(string); status == "failed" || status == "incomplete" || status == "cancelled" {
		return nil, &bpsError{"upstream completion has a conflicting status"}
	}
	nativeOutput, ok := response["output"].([]any)
	if !ok {
		nativeOutput = nil
	}
	finalCalls := map[string]bool{}
	for _, raw := range nativeOutput {
		if m, isMap := raw.(map[string]any); isMap {
			if t, _ := m["type"].(string); t == "function_call" || t == "custom_tool_call" {
				if callID, _ := m["call_id"].(string); callID != "" {
					finalCalls[callID] = true
				}
			}
		}
	}
	for _, item := range r.pending {
		callID, _ := item["call_id"].(string)
		if !finalCalls[callID] {
			return nil, &bpsError{"upstream completion omitted an original tool call"}
		}
	}
	translated := map[int]map[string]any{}
	validNative := []map[string]any{}
	seen := map[string]bool{}
	for index, raw := range nativeOutput {
		item, isMap := raw.(map[string]any)
		if !isMap {
			return nil, &bpsError{"upstream completion has an invalid output item"}
		}
		t, _ := item["type"].(string)
		if t != "function_call" && t != "custom_tool_call" {
			translated[index] = item
			continue
		}
		callID, _ := item["call_id"].(string)
		if seen[callID] {
			return nil, &bpsError{"upstream completion repeated a tool call id"}
		}
		seen[callID] = true
		call, reject := bpsTranslateTransport(item, r.allowed, r.custom)
		if call == nil {
			if t == "custom_tool_call" {
				return nil, &bpsError{"upstream custom tool call rejected: " + reject}
			}
			r.officeCalls = append(r.officeCalls, bpsDeepCopyMap(item))
			continue
		}
		translated[index] = call
		validNative = append(validNative, item)
	}
	events := [][2]any{}
	for index, item := range translated {
		t, _ := item["type"].(string)
		if t == "function_call" || t == "custom_tool_call" {
			r.clientCalls = append(r.clientCalls, item)
			field := "arguments"
			doneEvent := "response.function_call_arguments.done"
			if t == "custom_tool_call" {
				field = "input"
				doneEvent = "response.custom_tool_call_input.done"
			}
			addedName, addedPayload := r.visible("response.output_item.added", map[string]any{"output_index": float64(index), "item": item})
			events = append(events, [2]any{addedName, addedPayload})
			donePayload := map[string]any{"output_index": float64(index), "item_id": item["id"], "call_id": item["call_id"], "name": item["name"], field: item[field]}
			doneName, doneRewritten := r.visible(doneEvent, donePayload)
			events = append(events, [2]any{doneName, doneRewritten})
			finName, finPayload := r.visible("response.output_item.done", map[string]any{"output_index": float64(index), "item": item})
			events = append(events, [2]any{finName, finPayload})
		} else if _, mapped := r.indices[index]; !mapped {
			addedName, addedPayload := r.visible("response.output_item.added", map[string]any{"output_index": float64(index), "item": item})
			events = append(events, [2]any{addedName, addedPayload})
			finName, finPayload := r.visible("response.output_item.done", map[string]any{"output_index": float64(index), "item": item})
			events = append(events, [2]any{finName, finPayload})
		} else if !r.finished[index] {
			finName, finPayload := r.visible("response.output_item.done", map[string]any{"output_index": float64(index), "item": item})
			events = append(events, [2]any{finName, finPayload})
		}
	}
	for _, item := range validNative {
		if callID, _ := item["call_id"].(string); callID != "" {
			r.mem.remember(r.scope, callID, item, r.turnID, r.iteration)
		}
	}
	keys := make([]int, 0, len(translated))
	for index := range translated {
		keys = append(keys, index)
	}
	sort.Ints(keys)
	output := []any{}
	seenIdx := map[int]bool{}
	for _, index := range keys {
		_ = r.mapped(index)
		output = append(output, translated[index])
		seenIdx[index] = true
	}
	r.terminal = true
	response["status"] = "completed"
	response["output"] = output
	events = append(events, [2]any{"response.completed", map[string]any{"type": "response.completed", "response": response}})
	// 保留 usage 等顶层字段
	if usage, ok := payload["usage"]; ok {
		completed := events[len(events)-1][1].(map[string]any)
		completed["usage"] = usage
	}
	return events, nil
}

// ————— SSE 读取器包装（上游响应体 → 标准 Responses SSE） —————

var bpsSSEEventNamePattern = regexp.MustCompile(`^[a-z_.]+(\.[a-z_]+)*$`)

type bpsSSEReader struct {
	raw      io.Reader
	closer   io.Closer
	rewriter *bpsStreamRewriter
	scanner  *bufio.Scanner
	out      *bytes.Buffer
	done     bool
	err      error
	// clientModel 用于把响应里的模型名改回客户端请求值
	clientModel string
}

func newBPSSSEReader(body io.ReadCloser, rewriter *bpsStreamRewriter, clientModel string) *bpsSSEReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &bpsSSEReader{
		raw: body, closer: body, rewriter: rewriter,
		scanner: scanner, out: &bytes.Buffer{}, clientModel: clientModel,
	}
}

func (r *bpsSSEReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && !r.done {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.pump(); err != nil {
			r.err = err
			if r.out.Len() > 0 {
				break
			}
			return 0, err
		}
	}
	return r.out.Read(p)
}

func (r *bpsSSEReader) Close() error { return r.closer.Close() }

// pump 读取一个完整 SSE 事件（event:/data: 块），改写并序列化到 out。
func (r *bpsSSEReader) pump() error {
	eventName := ""
	var dataLines []string
	flush := func() error {
		defer func() { eventName = ""; dataLines = nil }()
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		if data == "[DONE]" {
			r.out.WriteString("data: [DONE]\n\n")
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return &bpsError{"upstream SSE contained malformed JSON: " + err.Error()}
		}
		event := eventName
		if event == "" || event == "message" {
			if t, ok := payload["type"].(string); ok {
				event = t
			}
		}
		events, err := r.rewriter.handle(event, payload)
		if err != nil {
			return err
		}
		for _, pair := range events {
			name := pair[0].(string)
			body := pair[1].(map[string]any)
			r.rewriteModelNames(body)
			encoded, err := json.Marshal(body)
			if err != nil {
				return &bpsError{"rewrite event marshal failed: " + err.Error()}
			}
			r.out.WriteString("event: " + name + "\n")
			r.out.WriteString("data: " + string(encoded) + "\n\n")
		}
		return nil
	}
	for r.scanner.Scan() {
		line := strings.TrimRight(r.scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			name := strings.TrimSpace(line[6:])
			if bpsSSEEventNamePattern.MatchString(name) {
				eventName = name
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimLeft(line[5:], " "))
			continue
		}
	}
	if err := r.scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	r.done = true
	return io.EOF
}

// rewriteModelNames 把事件内的模型名回写为客户端请求的原始模型。
func (r *bpsSSEReader) rewriteModelNames(body map[string]any) {
	if r.clientModel == "" {
		return
	}
	if response := payload2map(body["response"]); response != nil {
		response["model"] = r.clientModel
	}
}

// ————— 账号状态、健康度与路由判定 —————

// bpsIsTeamAccount 判定是否 Team/Business workspace 账号（plan_type 含
// team / business / enterprise）。BPS 代理仅对这类账号启用。
func bpsIsTeamAccount(account *Account) bool {
	if account == nil {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(account.GetCredential("plan_type")))
	switch {
	case plan == "":
		return false
	case strings.Contains(plan, "team"), strings.Contains(plan, "business"), strings.Contains(plan, "enterprise"):
		return true
	}
	return false
}

// bpsIsOAuthLikeAccount BPS 仅适用 ChatGPT OAuth 类（含 setup-token）非影子账号。
func bpsIsOAuthLikeAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// openAIBPSScopeEnabledFor 该账号所属范围（Team / 普通）的 BPS 开关：
// 后台热更新值优先，缺失回退 yaml（默认 false）。
func (s *OpenAIGatewayService) openAIBPSScopeEnabledFor(account *Account) bool {
	if s == nil || !bpsIsOAuthLikeAccount(account) {
		return false
	}
	team := bpsIsTeamAccount(account)
	fallback := false
	if s.cfg != nil {
		if team {
			fallback = s.cfg.Gateway.OpenBPS.TeamEnabled
		} else {
			fallback = s.cfg.Gateway.OpenBPS.PersonalEnabled
		}
	}
	if s.settingService == nil {
		return fallback
	}
	if team {
		return s.settingService.GetOpenABPSTeamEnabled(context.Background(), fallback)
	}
	return s.settingService.GetOpenABPSPersonalEnabled(context.Background(), fallback)
}

// openAIBPSAnyScopeEnabled 任一范围的 BPS 开关开启（负载均衡收窄的前置闸）。
func (s *OpenAIGatewayService) openAIBPSAnyScopeEnabled() bool {
	if s == nil {
		return false
	}
	if s.cfg != nil && (s.cfg.Gateway.OpenBPS.TeamEnabled || s.cfg.Gateway.OpenBPS.PersonalEnabled) && s.settingService == nil {
		return true
	}
	if s.settingService == nil {
		return false
	}
	return s.settingService.GetOpenABPSTeamEnabled(context.Background(), false) ||
		s.settingService.GetOpenABPSPersonalEnabled(context.Background(), false)
}

// openAIBPSRoutedFor 判定该请求是否走 BPS：账号所属范围（Team / 普通）的
// 开关开启。403 不封禁账号（只降优先级 +2），由调度偏好与健康度自然调节
// 流量，下次成功即自动恢复优先级。
func (s *OpenAIGatewayService) openAIBPSRoutedFor(account *Account) bool {
	if s == nil {
		return false
	}
	return s.openAIBPSScopeEnabledFor(account)
}

// bpsHealth 记录账号 BPS 最近一次成功 / 403 时间，驱动调度偏好。
type bpsHealth struct {
	lastOK  time.Time
	last403 time.Time
}

func (s *OpenAIGatewayService) bpsHealthMap() map[int64]bpsHealth {
	s.openAIBPSHealthOnce.Do(func() {
		s.openAIBPSHealthState = map[int64]bpsHealth{}
	})
	return s.openAIBPSHealthState
}

// openAIBPSAvailable 账号当前 BPS 是否可用：Team + 开关开，且最近一次
// 结果不是 403（成功后恢复）。无记录视为可用（乐观，403 会迅速纠正）。
// 「BPS 可用的账号调度优先级最高，高于会话粘滞。」
func (s *OpenAIGatewayService) openAIBPSAvailable(account *Account) bool {
	if s == nil || account == nil || !s.openAIBPSRoutedFor(account) {
		return false
	}
	s.openAIBPSHealthMu.Lock()
	defer s.openAIBPSHealthMu.Unlock()
	h, exists := s.bpsHealthMap()[account.ID]
	if !exists {
		return true
	}
	return !h.lastOK.Before(h.last403)
}

func (s *OpenAIGatewayService) noteOpenBPSResult(accountID int64, ok bool) {
	now := time.Now()
	s.openAIBPSHealthMu.Lock()
	defer s.openAIBPSHealthMu.Unlock()
	m := s.bpsHealthMap()
	h := m[accountID]
	if ok {
		h.lastOK = now
	} else {
		h.last403 = now
	}
	m[accountID] = h
}

// openAIBPSHandle403 上游 BPS 返回 403：记录健康度 + 优先级 +2（一次性）。
// 不封禁：后续请求仍可走 BPS，成功后自动恢复。
func (s *OpenAIGatewayService) openAIBPSHandle403(ctx context.Context, account *Account) {
	if s == nil || account == nil {
		return
	}
	s.noteOpenBPSResult(account.ID, false)
	if s.accountRepo == nil {
		return
	}
	if repo, ok := s.accountRepo.(interface {
		DemoteOpenBPSAccount(ctx context.Context, accountID int64) (bool, error)
	}); ok {
		demoted, err := repo.DemoteOpenBPSAccount(ctx, account.ID)
		if err != nil {
			logger.L().Warn("openai_bps demote failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return
		}
		if demoted {
			logger.L().Info("openai_bps account got 403, priority +2 (auto restore on next success)",
				zap.Int64("account_id", account.ID))
		}
	}
}

// openAIBPSHandleSuccess BPS 请求成功：恢复健康度；若曾因 403 降级则优先级 -2（一次性）。
func (s *OpenAIGatewayService) openAIBPSHandleSuccess(ctx context.Context, account *Account) {
	if s == nil || account == nil {
		return
	}
	needRestore := false
	s.openAIBPSHealthMu.Lock()
	if h, exists := s.bpsHealthMap()[account.ID]; exists && !h.last403.IsZero() && h.lastOK.Before(h.last403) {
		needRestore = true
	}
	s.openAIBPSHealthMu.Unlock()
	s.noteOpenBPSResult(account.ID, true)
	if !needRestore || s.accountRepo == nil {
		return
	}
	if repo, ok := s.accountRepo.(interface {
		RestoreOpenBPSAccount(ctx context.Context, accountID int64) (bool, error)
	}); ok {
		restored, err := repo.RestoreOpenBPSAccount(ctx, account.ID)
		if err != nil {
			logger.L().Warn("openai_bps restore failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return
		}
		if restored {
			logger.L().Info("openai_bps account recovered, priority restored",
				zap.Int64("account_id", account.ID))
		}
	}
}

// applyOpenBPSIdentityHeaders 按 Excel 加载项身份设置请求头。
func applyOpenBPSIdentityHeaders(h http.Header) {
	h.Set("origin", "https://bps.openai.com")
	h.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	h.Set("x-basispoints-auth-mode", "chatgpt")
	h.Set("x-openai-internal-basispoints-client-agent-profile", "excel")
	h.Set("x-openai-internal-basispoints-client-editor", "excel")
	h.Set("x-openai-internal-basispoints-client-host", "office")
	h.Set("x-openai-internal-basispoints-client-platform", "excel")
	h.Set("x-openai-internal-basispoints-client-platform-class", "PC")
	h.Set("x-openai-internal-basispoints-client-product", "basispoints-excel-plugin")
	h.Set("x-openai-internal-basispoints-client-runtime", "desktop")
	h.Set("x-openai-internal-basispoints-office-host", "Excel")
	h.Set("x-openai-internal-basispoints-office-platform", "PC")
	h.Set("x-stainless-arch", "unknown")
	h.Set("x-stainless-lang", "js")
	h.Set("x-stainless-os", "Unknown")
	h.Set("x-stainless-package-version", "6.31.0")
	h.Set("x-stainless-retry-count", "0")
	h.Set("x-stainless-runtime", "browser:chrome")
}

// ————— 网关钩子 —————

// openAIBPSMemoryInstance 服务级调用记忆（惰性初始化）。
func (s *OpenAIGatewayService) openAIBPSMemoryInstance() *bpsCallMemory {
	s.openAIBPSMemoryOnce.Do(func() {
		s.openAIBPSMemoryState = newBPSCallMemory()
	})
	return s.openAIBPSMemoryState
}

// bpsRouteState 经 gin context 在请求构造与响应包装之间传递。
type bpsRouteState struct {
	routed bool
	tools  []bpsClientTool
	scope  string
}

const bpsRouteStateContextKey = "openai_bps_route_state"

// openAIBPSScopeKey 以账号 + 会话为记忆隔离域。
func (s *OpenAIGatewayService) openAIBPSScopeKey(account *Account, source map[string]any) string {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	return fmt.Sprintf("%d:%s", accountID, bpsConversationIdentity(source))
}

// bpsPrepareRoute Forward 主路径的请求侧入口：返回翻译后的 body 与路由状态。
// 未启用 / 非 Team / 被封禁 / 非流式时原样返回（routed=false）。
func (s *OpenAIGatewayService) bpsPrepareRoute(c interface {
	Set(key string, value any)
}, account *Account, body []byte, isStream bool) []byte {
	if !s.openAIBPSRoutedFor(account) || !isStream {
		return body
	}
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return body
	}
	mem := s.openAIBPSMemoryInstance()
	scope := s.openAIBPSScopeKey(account, source)
	upstreamBody, _, _ := bpsPrepareBody(source, mem, scope, "")
	translated, err := json.Marshal(upstreamBody)
	if err != nil {
		return body
	}
	if setter, ok := c.(interface{ Set(string, any) }); ok {
		setter.Set(bpsRouteStateContextKey, &bpsRouteState{
			routed: true,
			tools:  mem.bindTools(scope, source),
			scope:  scope,
		})
	}
	return translated
}

// bpsRouteStateFrom 从 gin context 取路由状态（无则零值）。
func bpsRouteStateFrom(getter func(key string) (any, bool)) *bpsRouteState {
	if raw, ok := getter(bpsRouteStateContextKey); ok {
		if state, isState := raw.(*bpsRouteState); isState {
			return state
		}
	}
	return &bpsRouteState{}
}

// bpsRewriteUpstreamRequest 在 buildUpstreamRequest 尾部应用 BPS 改写。
func (s *OpenAIGatewayService) bpsRewriteUpstreamRequest(req *http.Request, body []byte, state *bpsRouteState) {
	if state == nil || !state.routed {
		return
	}
	req.URL.Scheme = "https"
	req.URL.Host = "bps.openai.com"
	req.URL.Path = "/basispoints/api/responses"
	req.Host = "bps.openai.com"
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	for _, key := range []string{
		"originator", "version", "session_id", "conversation_id", "openai-beta",
		"x-codex-beta-features", "x-codex-installation-id", "x-codex-window-id",
		"x-codex-turn-state", "x-codex-turn-metadata", "x-codex-thread-id",
		"x-codex-session-id", "x-codex-primary-account-id",
	} {
		req.Header.Del(key)
	}
	applyOpenBPSIdentityHeaders(req.Header)
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("content-type", "application/json")
}

// bpsWrapResponseBody 成功响应包一层 SSE 改写器（仅 BPS 路由的流式请求）。
func (s *OpenAIGatewayService) bpsWrapResponseBody(body io.ReadCloser, state *bpsRouteState, clientModel string) io.ReadCloser {
	if state == nil || !state.routed {
		return body
	}
	rewriter := newBPSStreamRewriter(state.tools, s.openAIBPSMemoryInstance(), state.scope, "", 0)
	return newBPSSSEReader(body, rewriter, clientModel)
}
