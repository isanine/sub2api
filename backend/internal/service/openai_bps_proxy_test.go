package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func bpsTestAccount(id int64, plan string) *Account {
	acc := &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "acc-1"},
	}
	if plan != "" {
		acc.Credentials["plan_type"] = plan
	}
	return acc
}

func bpsToolDecl() []any {
	return []any{map[string]any{
		"type":        "function",
		"name":        "shell",
		"description": "Run a shell command",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd": map[string]any{"type": "string", "description": "command"},
			},
			"required": []any{"cmd"},
		},
	}}
}

// 请求翻译：工具目录进 developer 提示词、模型别名、input 规范化。
func TestBPSPrepareBody(t *testing.T) {
	source := map[string]any{
		"model":        "gpt-6-sol",
		"tools":        bpsToolDecl(),
		"instructions": "You are a helpful assistant.",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "list files"},
		},
		"reasoning": map[string]any{"effort": "max"},
	}
	mem := newBPSCallMemory()
	body, _, _ := bpsPrepareBody(source, mem, "scope", "")
	require.Equal(t, "gpt-5.6-sol", body["model"])
	require.Equal(t, true, body["stream"])
	require.Equal(t, "xhigh", body["reasoning_effort"])
	rawInput := body["input"].([]any)
	require.GreaterOrEqual(t, len(rawInput), 3, "instructions + 协议提示 + 用户消息")
	items := make([]map[string]any, 0, len(rawInput))
	for _, raw := range rawInput {
		items = append(items, raw.(map[string]any))
	}
	first, _ := items[0]["content"].([]any)
	require.Contains(t, first, map[string]any{"type": "input_text", "text": "You are a helpful assistant."})
	joined := ""
	for _, item := range items {
		if item["role"] == "developer" {
			joined += bpsPartText(item["content"])
		}
	}
	require.Contains(t, joined, "Tool `shell`")
	require.Contains(t, joined, "run_officejs")
	// 用户消息保留在末尾
	last := items[len(items)-1]
	require.Equal(t, "user", last["role"])
	require.Contains(t, bpsPartText(last["content"]), "list files")
}

// 历史往返：客户端 function_call/output 经记忆翻译为 run_officejs 形态。
func TestBPSTranslateInputRoundtripWithMemory(t *testing.T) {
	mem := newBPSCallMemory()
	// 模拟流改写器已记住的 native 形态（上游返回的 run_officejs 调用）。
	native := map[string]any{
		"type":    "function_call",
		"id":      "fc_call_1",
		"call_id": "call_1",
		"name":    bpsTransportTool,
		"arguments": bpsDumps(map[string]any{
			"summary": "Run client tool shell",
			"code":    bpsDumps(map[string]any{"tool": "shell", "args": map[string]any{"cmd": "ls"}}),
		}),
		"status": "completed",
	}
	mem.remember("scope", "call_1", native, "turn", 1)

	source := map[string]any{
		"model": "gpt-5.6-sol",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "ls"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file1\nfile2"},
		},
	}
	translated := bpsTranslateInput(source["input"], mem, "scope")
	require.Len(t, translated, 3, "user + native call + rewritten output")
	// native 还原
	require.Equal(t, bpsTransportTool, translated[1]["name"])
	// output 转回 run_officejs 可接受的文本
	last := translated[2]
	require.Equal(t, "function_call_output", last["type"])
	require.Equal(t, "file1\nfile2", last["output"])
}

// 流改写器：completed 里的 run_officejs 调用还原为客户端 function_call 事件序列。
func TestBPSStreamRewriterCompleted(t *testing.T) {
	tools, _ := bpsDeclaredClientTools(map[string]any{"tools": bpsToolDecl()})
	mem := newBPSCallMemory()
	r := newBPSStreamRewriter(tools, mem, "scope", "turn", 1)

	// 增量事件：原生工具调用事件被吞掉（不外发参数）。
	native := map[string]any{
		"type": "function_call", "id": "fc_call_1", "call_id": "call_1",
		"name": bpsTransportTool,
		"arguments": bpsDumps(map[string]any{
			"summary": "s", "destructive": false, "references": []any{},
			"code": bpsDumps(map[string]any{"tool": "shell", "args": map[string]any{"cmd": "ls"}}),
		}),
	}
	events, err := r.handle("response.output_item.added", map[string]any{"output_index": float64(0), "item": native})
	require.NoError(t, err)
	require.Empty(t, events)

	completedPayload := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_1", "status": "completed",
			"output": []any{native},
			"usage":  map[string]any{"input_tokens": 10.0, "output_tokens": 5.0},
		},
	}
	events, err = r.handle("response.completed", completedPayload)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	// 最后是 completed，其 output 已是客户端形态。
	final, ok := events[len(events)-1][1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "response.completed", final["type"])
	resp := final["response"].(map[string]any)
	output := resp["output"].([]any)
	require.Len(t, output, 1)
	clientCall := output[0].(map[string]any)
	require.Equal(t, "function_call", clientCall["type"])
	require.Equal(t, "shell", clientCall["name"])
	require.JSONEq(t, `{"cmd":"ls"}`, clientCall["arguments"].(string))
	// 记忆已存：下次请求能还原。
	require.NotNil(t, mem.recall("scope", "call_1"))
	// 中间事件包含 added/done 序列。
	names := []string{}
	for _, e := range events[:len(events)-1] {
		names = append(names, e[0].(string))
	}
	require.Contains(t, names, "response.output_item.added")
	require.Contains(t, names, "response.function_call_arguments.done")
	require.Contains(t, names, "response.output_item.done")
}

// SSE 读取器：整条上游流被改写为标准 Responses SSE，模型名回写。
func TestBPSSSEReader(t *testing.T) {
	upstream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_9","model":"gpt-5.6-sol"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_9","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\n"
	tools, _ := bpsDeclaredClientTools(map[string]any{"tools": bpsToolDecl()})
	r := newBPSStreamRewriter(tools, newBPSCallMemory(), "scope", "turn", 1)
	reader := newBPSSSEReader(io.NopCloser(strings.NewReader(upstream)), r, "gpt-6-sol")
	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	text := string(out)
	require.Contains(t, text, "event: response.created")
	require.Contains(t, text, `"model":"gpt-6-sol"`)
	require.Contains(t, text, "event: response.completed")
	require.Contains(t, text, "output_text")
}

// 账号状态机：路由判定 + 403 降级 / 成功恢复（不封禁）。
func TestBPSRoutedDecision(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenBPS.Enabled = true

	team := bpsTestAccount(41, "team")
	personal := bpsTestAccount(42, "plus")
	require.True(t, bpsIsTeamAccount(team))
	require.False(t, bpsIsTeamAccount(personal))
	require.True(t, svc.openAIBPSRoutedFor(team))
	require.False(t, svc.openAIBPSRoutedFor(personal), "仅 Team 账号")
	// 无健康记录：可用（乐观）。
	require.True(t, svc.openAIBPSAvailable(team))
	// 开关关：一律不可用。
	svc.cfg.Gateway.OpenBPS.Enabled = false
	require.False(t, svc.openAIBPSRoutedFor(team))
	require.False(t, svc.openAIBPSAvailable(team))
}

// 403 → 不可用 + 降级；成功 → 恢复可用 + 还原优先级。
func TestBPSHealthDemoteAndRestore(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenBPS.Enabled = true
	repo := &bpsBlockRecorderRepo{}
	svc.accountRepo = repo
	team := bpsTestAccount(41, "team")

	require.True(t, svc.openAIBPSAvailable(team))
	svc.openAIBPSHandle403(context.Background(), team)
	require.False(t, svc.openAIBPSAvailable(team), "403 后调度不再偏好")
	require.Equal(t, []int64{41}, repo.blocked)

	// 再次 403：健康度更新，但成功前不重复触发（repo 层标记保证一次性）。
	svc.openAIBPSHandle403(context.Background(), team)
	require.Len(t, repo.blocked, 2) // 服务层每次都调，repo 原子保证只降一次

	// 成功：恢复可用 + 还原优先级。
	svc.openAIBPSHandleSuccess(context.Background(), team)
	require.True(t, svc.openAIBPSAvailable(team))
	require.Equal(t, []int64{41}, repo.unblocked)

	// 持续成功不再触发还原。
	svc.openAIBPSHandleSuccess(context.Background(), team)
	require.Len(t, repo.unblocked, 1)
}

// 403 处理器调用仓储接口（窄接口断言，mock 实现同一方法）。
type bpsBlockRecorderRepo struct {
	AccountRepository
	blocked   []int64
	unblocked []int64
}

func (r *bpsBlockRecorderRepo) DemoteOpenBPSAccount(ctx context.Context, accountID int64) (bool, error) {
	r.blocked = append(r.blocked, accountID)
	return true, nil
}

func (r *bpsBlockRecorderRepo) RestoreOpenBPSAccount(ctx context.Context, accountID int64) (bool, error) {
	r.unblocked = append(r.unblocked, accountID)
	return true, nil
}

// buildUpstreamRequest 的 BPS 改写：URL / Host / 身份头。
func TestBPSRewriteUpstreamRequest(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader([]byte("{}")))
	req.Host = "chatgpt.com"
	req.Header.Set("originator", "codex_cli_rs")
	state := &bpsRouteState{routed: true}
	svc := &OpenAIGatewayService{}
	svc.bpsRewriteUpstreamRequest(req, []byte(`{"model":"gpt-5.6-sol"}`), state)
	require.Equal(t, "bps.openai.com", req.Host)
	require.Equal(t, "https://bps.openai.com/basispoints/api/responses", req.URL.String())
	require.Equal(t, "excel", req.Header.Get("x-openai-internal-basispoints-client-editor"))
	require.Equal(t, "chatgpt", req.Header.Get("x-basispoints-auth-mode"))
	require.Empty(t, req.Header.Get("originator"))
	require.Equal(t, "text/event-stream", req.Header.Get("accept"))
}

// 设置键解析与开关读取。
func TestOpenABPSEnabledSetting(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	require.False(t, svc.openAIBPSEnabled())
	svc.cfg.Gateway.OpenBPS.Enabled = true
	require.True(t, svc.openAIBPSEnabled())
	var _ = json.Marshal
}

func TestBPSToolOutputEmptyIDGuard(t *testing.T) {
	output := bpsToolOutputItem(map[string]any{
		"type": "function_call_output", "call_id": "call_9", "id": "", "output": "ok",
	}, nil)
	_, hasID := output["id"]
	require.False(t, hasID, "空 id 不应写入")
	require.Equal(t, "ok", output["output"])

	// 非空 id 正常映射。
	output2 := bpsToolOutputItem(map[string]any{
		"type": "function_call_output", "call_id": "call_9", "id": "ctco_abc", "output": "ok",
	}, nil)
	require.Equal(t, "fc_abc", output2["id"])

	// bpsFCItemID 空串原样返回。
	require.Equal(t, "", bpsFCItemID(""))
}
