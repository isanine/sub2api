package admin

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSettingsCodexTicketScopeWriteReadAndHotReload(t *testing.T) {
	personalKey := service.SettingKeyOpenAICodexTicketPersonalEnabled
	teamKey := service.SettingKeyOpenAICodexTicketTeamEnabled
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{personalKey: "true", teamKey: "true"})
	require.True(t, h.settingService.GetOpenAICodexTicketPersonalEnabled(t.Context(), true))
	require.True(t, h.settingService.GetOpenAICodexTicketTeamEnabled(t.Context(), true))

	// 后台只关个人号。
	rec := doUpdateSettings(t, h, map[string]any{personalKey: false}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "false", repo.values[personalKey])
	require.Equal(t, "true", repo.values[teamKey])
	require.False(t, h.settingService.GetOpenAICodexTicketPersonalEnabled(t.Context(), true))
	require.True(t, h.settingService.GetOpenAICodexTicketTeamEnabled(t.Context(), true))

	// 恢复个人号、关 Team 号，热更新生效。
	rec = doUpdateSettings(t, h, map[string]any{personalKey: true, teamKey: false}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, h.settingService.GetOpenAICodexTicketPersonalEnabled(t.Context(), false))
	require.False(t, h.settingService.GetOpenAICodexTicketTeamEnabled(t.Context(), true))

	// 响应包含两个细分开关字段。
	require.Contains(t, rec.Body.String(), `"openai_codex_ticket_personal_enabled":true`)
	require.Contains(t, rec.Body.String(), `"openai_codex_ticket_team_enabled":false`)
}
