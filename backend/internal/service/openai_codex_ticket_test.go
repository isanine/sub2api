package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func fakeCodexTicketState(n int) string {
	if n < len(openAICodexTicketStatePrefix) {
		return strings.Repeat("A", n)
	}
	return openAICodexTicketStatePrefix + strings.Repeat("B", n-len(openAICodexTicketStatePrefix))
}

func ticketTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "acc-1"},
	}
}

func ticketTestService(t *testing.T, cfg config.OpenAICodexTicketConfig) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{OpenAICodexTicket: cfg},
		},
	}
}

func boolPtrForTest(v bool) *bool { return &v }

func storeTestTicket(svc *OpenAIGatewayService, a *Account, model string, n int) {
	svc.storeOpenAICodexTicket(context.Background(), a, &openAICodexTicket{
		AccountID:  a.ID,
		Model:      model,
		State:      fakeCodexTicketState(n),
		Length:     n,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
}

func applyTicket(t *testing.T, svc *OpenAIGatewayService, a *Account, model string) http.Header {
	t.Helper()
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), a, model, h))
	return h
}

// ————— 注入：有票覆盖、无票透传、永不拦截 —————

func TestApplyOpenAICodexTicket_ReplacesHeader(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600})
	account := ticketTestAccount(41)
	storeTestTicket(svc, account, "gpt-6-astra", 292)

	h := applyTicket(t, svc, account, "gpt-6-astra")
	require.Equal(t, fakeCodexTicketState(292), h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_NoTicketPassesThroughAndNeverBlocks(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600})
	account := ticketTestAccount(41)

	// 无票：客户端自带头原样透传，不返回错误（被动模式无 fail_closed）。
	h := applyTicket(t, svc, account, "gpt-6-astra")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))

	// 票过期：同样透传。
	storeTestTicket(svc, account, "gpt-6-astra", 292)
	if raw, ok := svc.openaiCodexTickets.Load(openAICodexTicketKey(41, "gpt-6-astra")); ok {
		ticket := raw.(*openAICodexTicket)
		ticket.ExpiresAt = time.Now().Add(-time.Minute)
	}
	h = applyTicket(t, svc, account, "gpt-6-astra")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))

	// 形状不符（312）：透传不注入。
	storeTestTicket(svc, account, "gpt-6-astra", 312)
	h = applyTicket(t, svc, account, "gpt-6-astra")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_DoesNotReuseOtherModelOrAccount(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600})
	a := ticketTestAccount(41)
	b := ticketTestAccount(42)
	storeTestTicket(svc, a, "gpt-6-astra", 292)

	// 其他模型：不动。
	h := applyTicket(t, svc, a, "gpt-5.5")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	// 其他账号：不动。
	h = applyTicket(t, svc, b, "gpt-6-astra")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	// 本账号本模型：注入。
	h = applyTicket(t, svc, a, "gpt-6-astra")
	require.Len(t, h.Get(openAICodexTurnStateHeader), 292)
}

func TestApplyOpenAICodexTicket_DisabledNoop(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false})
	h := applyTicket(t, svc, ticketTestAccount(41), "gpt-6-astra")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

func TestLookupOpenAICodexTicket_PrefersNewerExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600})
	account := ticketTestAccount(41)
	oldState := fakeCodexTicketState(292)
	newState := openAICodexTicketStatePrefix + strings.Repeat("C", 286)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      oldState,
		Length:     292,
		CapturedAt: time.Now().Add(-30 * time.Minute),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): &openAICodexTicket{
		Model:      "gpt-6-astra",
		State:      newState,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}}
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, newState, got.State)
	require.True(t, got.valid(time.Now(), 292))
}

func TestExtractOpenAICodexTicketModel(t *testing.T) {
	require.Equal(t, "gpt-6-astra", extractOpenAICodexTicketModel([]byte(`{"model":" gpt-6-astra "}`)))
	require.Equal(t, "", extractOpenAICodexTicketModel([]byte(`{}`)))
}

func TestOpenAICodexTicket_RequiresActualLengthAndExpiry(t *testing.T) {
	now := time.Now()
	base := &openAICodexTicket{
		Model: "gpt-6-astra", State: fakeCodexTicketState(292), Length: 292,
		CapturedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	require.True(t, base.valid(now, 292))
	require.False(t, base.valid(now, 332), "长度与目标不符")
	require.False(t, base.valid(now.Add(2*time.Hour), 292), "已过期")
	bad := *base
	bad.Length = 0
	require.False(t, bad.valid(now, 292), "Length 字段必须与实际长度一致")
}

// ————— 套餐 → 合格长度 —————

func TestOpenAICodexTicketTargetLengthByPlanType(t *testing.T) {
	cases := []struct {
		planType string
		want     int
	}{
		{"", 292},
		{"free", 292},
		{"plus", 292},
		{"pro", 292},
		{"team", 332},
		{"Team", 332},
		{"business", 332},
		{"self_serve_business_prolite", 332},
		{"enterprise", 332},
	}
	for _, tc := range cases {
		account := ticketTestAccount(41)
		if tc.planType != "" {
			account.Credentials["plan_type"] = tc.planType
		}
		require.Equal(t, tc.want, openAICodexTicketTargetLength(account, 292), "plan_type=%q", tc.planType)
	}
	team := ticketTestAccount(42)
	team.Credentials["plan_type"] = "team"
	require.Equal(t, 332, openAICodexTicketTargetLength(team, 0))
	require.Equal(t, 292, openAICodexTicketTargetLength(ticketTestAccount(43), 0))
	require.Equal(t, 292, openAICodexTicketTargetLength(nil, 292))
}

// ————— 被动捕获 —————

func captureResponseHeader(n int) http.Header {
	h := http.Header{}
	if n > 0 {
		h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(n))
	}
	return h
}

func TestCaptureOpenAICodexTicket_QualifiedStateStored(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600, Models: []string{"gpt-6-astra"}})
	personal := ticketTestAccount(41)
	svc.captureOpenAICodexTicketFromResponse(personal, "gpt-6-astra", captureResponseHeader(292))
	require.True(t, svc.lookupOpenAICodexTicket(personal, "gpt-6-astra").valid(time.Now(), 292))

	team := ticketTestAccount(42)
	team.Credentials["plan_type"] = "team"
	svc.captureOpenAICodexTicketFromResponse(team, "gpt-6-astra", captureResponseHeader(332))
	require.True(t, svc.lookupOpenAICodexTicket(team, "gpt-6-astra").valid(time.Now(), 332))
}

func TestCaptureOpenAICodexTicket_IgnoresUnqualifiedShapes(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600, Models: []string{"gpt-6-astra"}})
	for _, n := range []int{0, 100, 291, 312, 332, 356} {
		account := ticketTestAccount(41)
		svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", captureResponseHeader(n))
		require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"), "个人号长度 %d 不应落库", n)
	}
	// 非门控模型 / 功能关闭 / 细分关闭 / 影子号：一律忽略。
	svc.captureOpenAICodexTicketFromResponse(ticketTestAccount(42), "gpt-5.5", captureResponseHeader(292))
	require.Nil(t, svc.lookupOpenAICodexTicket(ticketTestAccount(42), "gpt-5.5"))
	off := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false})
	off.captureOpenAICodexTicketFromResponse(ticketTestAccount(43), "gpt-6-astra", captureResponseHeader(292))
	require.Nil(t, off.lookupOpenAICodexTicket(ticketTestAccount(43), "gpt-6-astra"))
	teamOff := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, TargetLength: 292, Models: []string{"gpt-6-astra"}, EnabledTeam: boolPtrForTest(false),
	})
	team := ticketTestAccount(44)
	team.Credentials["plan_type"] = "team"
	teamOff.captureOpenAICodexTicketFromResponse(team, "gpt-6-astra", captureResponseHeader(332))
	require.Nil(t, teamOff.lookupOpenAICodexTicket(team, "gpt-6-astra"))
}

// ————— 个人 / Team 细分开关（注入侧） —————

func TestOpenAICodexTicketScopeSwitch_GatesPerAccountType(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}
	personal := ticketTestAccount(71)
	team := ticketTestAccount(72)
	team.Credentials["plan_type"] = "team"

	t.Run("nil 细分开关默认两类全开", func(t *testing.T) {
		svc := ticketTestService(t, cfg)
		storeTestTicket(svc, personal, "gpt-6-astra", 292)
		storeTestTicket(svc, team, "gpt-6-astra", 332)
		require.Len(t, applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader), 292)
		require.Len(t, applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader), 332)
	})

	t.Run("关闭个人号只影响个人号", func(t *testing.T) {
		c := cfg
		c.EnabledPersonal = boolPtrForTest(false)
		svc := ticketTestService(t, c)
		storeTestTicket(svc, personal, "gpt-6-astra", 292)
		storeTestTicket(svc, team, "gpt-6-astra", 332)
		require.Equal(t, "client-state", applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader))
		require.Len(t, applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader), 332)
	})

	t.Run("关闭 Team 号只影响 Team 号", func(t *testing.T) {
		c := cfg
		c.EnabledTeam = boolPtrForTest(false)
		svc := ticketTestService(t, c)
		storeTestTicket(svc, personal, "gpt-6-astra", 292)
		storeTestTicket(svc, team, "gpt-6-astra", 332)
		require.Len(t, applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader), 292)
		require.Equal(t, "client-state", applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader))
	})

	t.Run("总开关关闭时细分开关无效", func(t *testing.T) {
		c := cfg
		c.Enabled = false
		c.EnabledPersonal = boolPtrForTest(true)
		c.EnabledTeam = boolPtrForTest(true)
		svc := ticketTestService(t, c)
		storeTestTicket(svc, personal, "gpt-6-astra", 292)
		require.Equal(t, "client-state", applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader))
	})
}

// ————— 管理端状态 —————

func TestOpenAICodexTicketStatuses_ReportsRemainingTTLAndScope(t *testing.T) {
	statusCfg := config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}
	account := ticketTestAccount(41)
	exp := time.Now().Add(30 * time.Minute)
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
		"model": "gpt-6-astra", "state": fakeCodexTicketState(292), "length": 292,
		"captured_at": time.Now().Format(time.RFC3339Nano), "expires_at": exp.Format(time.RFC3339Nano),
	}}
	statuses := OpenAICodexTicketStatuses(account, statusCfg, time.Now())
	require.Len(t, statuses, 2)
	var astra *OpenAICodexTicketStatus
	for i := range statuses {
		if statuses[i].Model == "gpt-6-astra" {
			astra = &statuses[i]
		}
	}
	require.NotNil(t, astra)
	require.True(t, astra.Ready)
	require.Equal(t, 292, astra.Length)
	require.InDelta(t, 1800, astra.RemainingSeconds, 5)

	// 细分关闭：该范围账号不展示。
	offCfg := statusCfg
	offCfg.EnabledPersonal = boolPtrForTest(false)
	require.Empty(t, OpenAICodexTicketStatuses(account, offCfg, time.Now()))
}

// ————— extra 键管理与脱敏 —————

func TestMergeOpenAICodexTicketExtraPreservesPersistedTickets(t *testing.T) {
	ticket := map[string]any{"model": "gpt-6-astra", "state": fakeCodexTicketState(292)}
	merged := MergeOpenAICodexTicketExtra(
		map[string]any{"email": "a@b.c", openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{"stale": true}},
		map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): ticket},
	)
	require.Equal(t, "a@b.c", merged["email"])
	require.Equal(t, ticket, merged[openAICodexTicketExtraKey("gpt-6-astra")])
}

func TestRedactOpenAICodexTicketExtraStripsTicketMaterial(t *testing.T) {
	redacted := RedactOpenAICodexTicketExtra(map[string]any{
		"email": "a@b.c",
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{"state": "secret"},
		"codex_harvest_proxy_url":                "http://user:pass@host:1",
	})
	require.Equal(t, "a@b.c", redacted["email"])
	_, hasTicket := redacted[openAICodexTicketExtraKey("gpt-6-astra")]
	require.False(t, hasTicket)
	_, hasProxy := redacted["codex_harvest_proxy_url"]
	require.False(t, hasProxy)
}

func TestParseOpenAICodexTicketFromAnyFillLength(t *testing.T) {
	raw := map[string]any{"model": "gpt-6-astra", "state": fakeCodexTicketState(292)}
	b, _ := json.Marshal(raw)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	ticket := parseOpenAICodexTicketFromAny(7, "gpt-6-astra", m)
	require.NotNil(t, ticket)
	require.Equal(t, 292, ticket.Length)
	require.Equal(t, int64(7), ticket.AccountID)
	require.Nil(t, parseOpenAICodexTicketFromAny(7, "m", nil))
}

// ————— 连续未捕获 → 一次性优先级降级 —————

type demoteRecorderRepo struct {
	AccountRepository
	demoted []int64
}

func (r *demoteRecorderRepo) DemoteCodexTicketPriority(ctx context.Context, accountID int64) (bool, error) {
	r.demoted = append(r.demoted, accountID)
	return true, nil
}

func (r *demoteRecorderRepo) UpdateExtra(ctx context.Context, accountID int64, extra map[string]any) error {
	return nil
}

func missResponse() http.Header { return http.Header{} }

func TestCaptureOpenAICodexTicket_DemotesAfterConsecutiveMisses(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{
		Enabled: true, TargetLength: 292, TTLSeconds: 3600,
		Models: []string{"gpt-6-astra", "gpt-5.6-sol"},
		PriorityDemoteThreshold: 100,
	}
	repo := &demoteRecorderRepo{}
	svc := ticketTestService(t, cfg)
	svc.accountRepo = repo
	account := ticketTestAccount(41)

	// 99 次未捕获：不降级。
	for i := 0; i < 99; i++ {
		svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", missResponse())
	}
	require.Empty(t, repo.demoted)

	// 第 100 次且两个模型都无票：降级一次。
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", missResponse())
	require.Equal(t, []int64{41}, repo.demoted)

	// 捕获成功后计数清零：再 99 次 miss 不会再次降级。
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", captureResponseHeader(292))
	require.True(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra").valid(time.Now(), 292))
	for i := 0; i < 99; i++ {
		svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", missResponse())
	}
	require.Len(t, repo.demoted, 1)

	// 已降过级（repo 返回 false 场景由 SQL 保证）；阈值 0 = 关闭。
	offCfg := cfg
	offCfg.PriorityDemoteThreshold = 0
	offRepo := &demoteRecorderRepo{}
	offSvc := ticketTestService(t, offCfg)
	offSvc.accountRepo = offRepo
	off := ticketTestAccount(42)
	for i := 0; i < 300; i++ {
		offSvc.captureOpenAICodexTicketFromResponse(off, "gpt-6-astra", missResponse())
	}
	require.Empty(t, offRepo.demoted)
}

func TestCaptureOpenAICodexTicket_NoDemoteWhileOtherModelHasTicket(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{
		Enabled: true, TargetLength: 292, TTLSeconds: 3600,
		Models: []string{"gpt-6-astra", "gpt-5.6-sol"},
		PriorityDemoteThreshold: 100,
	}
	repo := &demoteRecorderRepo{}
	svc := ticketTestService(t, cfg)
	svc.accountRepo = repo
	account := ticketTestAccount(41)

	// gpt-5.6-sol 持有有效票，gpt-6-astra 连续 100 次 miss：不降级（另一模型有票）。
	storeTestTicket(svc, account, "gpt-5.6-sol", 292)
	for i := 0; i < 100; i++ {
		svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", missResponse())
	}
	require.Empty(t, repo.demoted)

	// 持票模型的 miss 不计数（模型仍有有效票时）。
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-5.6-sol", missResponse())
	require.Empty(t, repo.demoted)
}
