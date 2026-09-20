package service

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix  = "codex_turn_ticket:"
	openAICodexTicketStatePrefix     = "gAAAAA"
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"

	// 门票合格长度按账号套餐区分：个人号（free/plus/pro）10 块 / 292 字符；
	// Team/Business workspace 12 块 / 332 字符。11 块 / 312 与 13 块 / 356
	// 是过渡形状，不作为合格值。
	openAICodexTicketPersonalTargetLength = 292
	openAICodexTicketTeamTargetLength     = 332
)

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts"`
}

func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = openAICodexTicketPersonalTargetLength
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 3600
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	return cfg
}

// openAICodexTicketGatedModel 判定模型是否在门票覆盖范围内。
func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model            string     `json:"model"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	// 细分开关：该账号所属范围（个人 / Team）未开启时不参与门票捕获与注入。
	if isOpenAICodexTicketTeamAccount(account) {
		if !cfg.TeamEnabled() {
			return nil
		}
	} else if !cfg.PersonalEnabled() {
		return nil
	}
	models := cfg.Models
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	targetLen := openAICodexTicketTargetLength(account, cfg.TargetLength)
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		status := OpenAICodexTicketStatus{Model: model}
		ticket := parseOpenAICodexTicketFromAny(0, model, nil)
		if account != nil && account.Extra != nil {
			ticket = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
		}
		if ticket.valid(now, targetLen) {
			status.Ready = true
			status.Length = ticket.Length
			remaining := int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			status.RemainingSeconds = remaining
			exp := ticket.ExpiresAt
			status.ExpiresAt = &exp
		}
		out = append(out, status)
	}
	return out
}

func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

// openAICodexTicketScopeEnabledContext 判定票务检测是否对该账号生效：
// 总开关开启后，个人号 / Team 号各自受独立细分开关控制（后台可热更），
// 未配置细分开关时两类都默认开启（兼容历史行为）。
func (s *OpenAIGatewayService) openAICodexTicketScopeEnabledContext(ctx context.Context, account *Account) bool {
	if s == nil || !s.openAICodexTicketEnabledContext(ctx) {
		return false
	}
	team := isOpenAICodexTicketTeamAccount(account)
	fallback := true
	if s.cfg != nil {
		if team {
			fallback = s.cfg.Gateway.OpenAICodexTicket.TeamEnabled()
		} else {
			fallback = s.cfg.Gateway.OpenAICodexTicket.PersonalEnabled()
		}
	}
	if s.settingService != nil {
		if team {
			return s.settingService.GetOpenAICodexTicketTeamEnabled(ctx, fallback)
		}
		return s.settingService.GetOpenAICodexTicketPersonalEnabled(ctx, fallback)
	}
	return fallback
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

// isOpenAICodexTicketTeamAccount 判断是否 Team/Business workspace 账号。
// Team/Business 的 x-codex-turn-state 门票是 12 块 / 332 字符，与个人号的
// 10 块 / 292 不同；校验口径必须跟随账号套餐，否则 Team 号捕获到的合法
// 332 会被当成不合格丢弃。
func isOpenAICodexTicketTeamAccount(account *Account) bool {
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

// openAICodexTicketTargetLength 返回该账号的门票合格长度：
// Team/Business 固定 332；其余（个人 free/plus/pro 及未识别套餐）用配置
// target_length，默认 292。
func openAICodexTicketTargetLength(account *Account, cfgTargetLength int) int {
	if isOpenAICodexTicketTeamAccount(account) {
		return openAICodexTicketTeamTargetLength
	}
	if cfgTargetLength <= 0 {
		return openAICodexTicketPersonalTargetLength
	}
	return cfgTargetLength
}

func (s *OpenAIGatewayService) lookupOpenAICodexTicket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	targetLen := openAICodexTicketTargetLength(account, s.openAICodexTicketConfig().TargetLength)
	now := time.Now()
	var mem *openAICodexTicket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicket)
	}
	var extra *openAICodexTicket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
	}
	if extra.valid(now, targetLen) && (mem == nil || extra.CapturedAt.After(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.valid(now, targetLen) {
		return mem
	}
	if extra != nil {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem != nil {
		s.openaiCodexTickets.Delete(key)
	}
	return nil
}

func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model): ticket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

// openAICodexTicketDemotedExtraKey 标记账号已因连续未捕获门票降过优先级
// （只降一次，不重复降）。
const openAICodexTicketDemotedExtraKey = "codex_ticket_priority_demoted"

// captureOpenAICodexTicketFromResponse 被动票务检测：所有请求正常放行，
// 不主动打票。上游真实业务响应头携带 x-codex-turn-state 且形状合格
// （个人 292 / Team 332、gAAAAA 前缀）时按（账号, 模型）捕获落库，
// 供后续请求注入。形状不合格或非门控模型时静默忽略，不影响响应。
//
// 连续未捕获计数与一次性降级：门控模型响应未捕获且该模型无有效票时计数 +1，
// 捕获成功清零；达到 priority_demote_threshold（默认 100）且所有门控模型
// 都无有效票时，账号优先级 +1（数值越大调度越靠后），已降过级的不再降。
func (s *OpenAIGatewayService) captureOpenAICodexTicketFromResponse(account *Account, model string, upstream http.Header) {
	if s == nil || upstream == nil || !isOpenAICodexTicketAccount(account) {
		return
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return
	}
	if !s.openAICodexTicketScopeEnabledContext(context.Background(), account) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	state := extractOpenAICodexTurnState(upstream)
	targetLen := openAICodexTicketTargetLength(account, cfg.TargetLength)
	if state != "" && len(state) == targetLen && strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		now := time.Now()
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      state,
			Length:     len(state),
			CapturedAt: now,
			ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			Attempts:   1,
		}
		// 已有一张同 key 且更新的票（并发捕获）时保留新值即可；捕获本身低频。
		s.storeOpenAICodexTicket(context.Background(), account, ticket)
		s.resetOpenAICodexTicketMissCount(account.ID)
		logger.L().Info("openai_codex_ticket captured",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Int("length", ticket.Length),
			zap.String("mode", "passive"),
		)
		return
	}
	// 未捕获：该模型仍持有有效票（旧票未过期）时不计数；否则连续 miss +1。
	if ticket := s.lookupOpenAICodexTicket(account, model); ticket.valid(time.Now(), targetLen) {
		return
	}
	s.noteOpenAICodexTicketMiss(account, cfg)
}

func (s *OpenAIGatewayService) resetOpenAICodexTicketMissCount(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiCodexTicketMissCounts.Delete(accountID)
}

func (s *OpenAIGatewayService) noteOpenAICodexTicketMiss(account *Account, cfg config.OpenAICodexTicketConfig) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	threshold := cfg.PriorityDemoteThreshold
	if threshold <= 0 {
		return // 自动降级关闭
	}
	raw, _ := s.openaiCodexTicketMissCounts.LoadOrStore(account.ID, new(atomic.Int64))
	counter, _ := raw.(*atomic.Int64)
	if counter == nil {
		return
	}
	if counter.Add(1) < int64(threshold) {
		return
	}
	// 达到阈值：仅当所有门控模型都无有效票时降级（例如 gpt-6-astra 与
	// gpt-5.6-sol 同时都没有票）；任一模型仍有票则重置计数继续观察。
	counter.Store(0)
	if s.accountRepo == nil {
		return
	}
	now := time.Now()
	for _, m := range cfg.Models {
		m = normalizeOpenAICodexTicketModel(m)
		if m == "" {
			continue
		}
		if t := s.lookupOpenAICodexTicket(account, m); t.valid(now, openAICodexTicketTargetLength(account, cfg.TargetLength)) {
			return
		}
	}
	demoted, err := s.accountRepo.DemoteCodexTicketPriority(context.Background(), account.ID)
	if err != nil {
		logger.L().Warn("openai_codex_ticket demote failed",
			zap.Int64("account_id", account.ID), zap.Error(err))
		return
	}
	if demoted {
		logger.L().Info("openai_codex_ticket priority demoted",
			zap.Int64("account_id", account.ID),
			zap.Int("consecutive_misses", threshold),
			zap.String("reason", "no ticket captured for any gated model"),
		)
	}
}

// applyOpenAICodexTicket 在出站请求上注入已捕获的门票。
// 被动模式下永不拦截：无票时保持客户端自带头原样透传，返回值恒为 nil。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketScopeEnabledContext(ctx, account) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg.TargetLength)) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
	}
	return nil
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent capture cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}

// Credential shadows do not own tickets; their forwarding policy is untouched
// by ticket capture/injection.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// BoolPtr 返回 bool 的指针，供配置结构（*bool 细分开关）覆写使用。
func BoolPtr(v bool) *bool { return &v }
