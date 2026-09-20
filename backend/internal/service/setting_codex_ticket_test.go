package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type codexTicketSettingRepo struct {
	*codexPolicyMigrationRepoStub
	err error
}

func (r *codexTicketSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.codexPolicyMigrationRepoStub.GetValue(ctx, key)
}

func TestCodexTicketEnabledRuntimeSettingOverridesYaml(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}}
	settings := NewSettingService(repo, &config.Config{})
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false})
	svc.settingService = settings
	account := ticketTestAccount(41)
	storeTestTicket(svc, account, "gpt-6-astra", 292)

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	require.False(t, svc.openAICodexTicketEnabled())
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))

	repo.values[SettingKeyOpenAICodexTicketEnabled] = "true"
	settings.InvalidateOpenAICodexTicketEnabledCache()
	require.True(t, svc.openAICodexTicketEnabled())
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, fakeCodexTicketState(292), h.Get(openAICodexTurnStateHeader))

	repo.values[SettingKeyOpenAICodexTicketEnabled] = "false"
	settings.InvalidateOpenAICodexTicketEnabledCache()
	require.False(t, svc.openAICodexTicketEnabled())
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

func TestCodexTicketScopeRuntimeSettingsDefaultOnAndOverride(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}}
	settings := NewSettingService(repo, &config.Config{})
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true})
	svc.settingService = settings

	personal := ticketTestAccount(41)
	team := ticketTestAccount(42)
	team.Credentials["plan_type"] = "team"
	storeTestTicket(svc, personal, "gpt-6-astra", 292)
	storeTestTicket(svc, team, "gpt-6-astra", 332)

	// 键缺失：两类默认开启（兼容历史行为）。
	require.Len(t, applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader), 292)
	require.Len(t, applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader), 332)

	// 后台只关个人号。
	repo.values[SettingKeyOpenAICodexTicketPersonalEnabled] = "false"
	settings.InvalidateOpenAICodexTicketPersonalCache()
	require.Equal(t, "client-state", applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader))
	require.Len(t, applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader), 332)

	// 后台只关 Team 号（个人号恢复）。
	repo.values[SettingKeyOpenAICodexTicketPersonalEnabled] = "true"
	repo.values[SettingKeyOpenAICodexTicketTeamEnabled] = "false"
	settings.InvalidateOpenAICodexTicketPersonalCache()
	settings.InvalidateOpenAICodexTicketTeamCache()
	require.Len(t, applyTicket(t, svc, personal, "gpt-6-astra").Get(openAICodexTurnStateHeader), 292)
	require.Equal(t, "client-state", applyTicket(t, svc, team, "gpt-6-astra").Get(openAICodexTurnStateHeader))
}

func TestCodexTicketSettingsRefreshDoesNotMutateSharedConfig(t *testing.T) {
	cfg := &config.Config{}
	svc := NewSettingService(&codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{SettingKeyOpenAICodexTicketEnabled: "true"}}}, cfg)
	svc.refreshCachedSettings(&SystemSettings{OpenAICodexTicketEnabled: true, OpenAICodexTicketPersonalEnabled: true, OpenAICodexTicketTeamEnabled: true})
	require.False(t, cfg.Gateway.OpenAICodexTicket.Enabled, "runtime settings must not write the shared immutable startup configuration")
	require.True(t, svc.GetOpenAICodexTicketEnabled(context.Background(), false))
	// 细分键缺失时回退调用方传入的 yaml 值（生产调用方传 PersonalEnabled()/TeamEnabled()，默认 true）。
	require.False(t, svc.GetOpenAICodexTicketPersonalEnabled(context.Background(), false))
	svc.InvalidateOpenAICodexTicketPersonalCache()
	require.True(t, svc.GetOpenAICodexTicketPersonalEnabled(context.Background(), true))
	svc.InvalidateOpenAICodexTicketTeamCache()
	require.True(t, svc.GetOpenAICodexTicketTeamEnabled(context.Background(), true))
}

func TestCaptureOpenAICodexTicketRuntimeSettingGate(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}}
	settings := NewSettingService(repo, &config.Config{})
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, Models: []string{"gpt-6-astra"}})
	svc.settingService = settings
	account := ticketTestAccount(41)

	// 总开关关：响应带合格票也不捕获。
	header := http.Header{}
	header.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", header)
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))

	// 总开关开、个人细分关：仍不捕获。
	repo.values[SettingKeyOpenAICodexTicketEnabled] = "true"
	repo.values[SettingKeyOpenAICodexTicketPersonalEnabled] = "false"
	settings.InvalidateOpenAICodexTicketEnabledCache()
	settings.InvalidateOpenAICodexTicketPersonalCache()
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", header)
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))

	// 个人细分恢复：捕获成功。
	repo.values[SettingKeyOpenAICodexTicketPersonalEnabled] = "true"
	settings.InvalidateOpenAICodexTicketPersonalCache()
	svc.captureOpenAICodexTicketFromResponse(account, "gpt-6-astra", header)
	require.True(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra").valid(time.Now(), 292))
}
