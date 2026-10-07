package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type highTTFTCounterStub struct {
	count int64
	reset int
}

func (c *highTTFTCounterStub) IncrementHighTTFTCount(ctx context.Context, accountID int64, windowMinutes int) (int64, error) {
	c.count++
	return c.count, nil
}

func (c *highTTFTCounterStub) ResetHighTTFTCount(ctx context.Context, accountID int64) error {
	c.reset++
	c.count = 0
	return nil
}

type highTTFTSettingsRepo struct {
	SettingRepository
	value string
}

func (r *highTTFTSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyHighTTFTSettings && r.value != "" {
		return r.value, nil
	}
	return "", ErrSettingNotFound
}

func (r *highTTFTSettingsRepo) Set(ctx context.Context, key, value string) error {
	r.value = value
	return nil
}

type highTTFTRepoStub struct {
	AccountRepository
	paused  []int64
	errored []int64
}

func (r *highTTFTRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.paused = append(r.paused, id)
	return nil
}

func (r *highTTFTRepoStub) SetError(ctx context.Context, id int64, errorMsg string) error {
	r.errored = append(r.errored, id)
	return nil
}

func newHighTTFTTestService(t *testing.T, settingsJSON string) (*RateLimitService, *highTTFTCounterStub, *highTTFTRepoStub) {
	t.Helper()
	settingsRepo := &highTTFTSettingsRepo{value: settingsJSON}
	settingSvc := NewSettingService(settingsRepo, nil)
	repo := &highTTFTRepoStub{}
	counter := &highTTFTCounterStub{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetHighTTFTCounterCache(counter)
	svc.SetSettingService(settingSvc)
	return svc, counter, repo
}

func highTTFTSettingsJSON(action string, thresholdCount int) string {
	return `{"enabled":true,"action":"` + action + `","temp_unsched_minutes":5,"threshold_count":` +
		string(rune('0'+thresholdCount)) + `,"threshold_window_minutes":10,"ttft_threshold_seconds":30}`
}

// 连续判定：正常首字重置计数；连续 3 次高首字触发临时不可调度并清零计数。
func TestHighTTFTConsecutiveResetAndTrigger(t *testing.T) {
	svc, counter, repo := newHighTTFTTestService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":5,"threshold_count":3,"threshold_window_minutes":10,"ttft_threshold_seconds":30}`)
	account := &Account{ID: 41}

	// 两次高首字（>30s）：计数累计，未达阈值不处置。
	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 45))
	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 60))
	require.Empty(t, repo.paused)

	// 出现正常首字（<=30s）：连续计数清零。
	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 10))
	require.Equal(t, 1, counter.reset)

	// 重新累计 2 次仍未达 3 次：不处置。
	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 45))
	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 45))
	require.Empty(t, repo.paused)

	// 第 3 次连续高首字：达到阈值，触发临时不可调度并清零计数。
	require.True(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 120))
	require.Equal(t, []int64{41}, repo.paused)
	require.Equal(t, 2, counter.reset)
}

// 403 之外的 error 动作：达到阈值标记错误状态。
func TestHighTTFTErrorAction(t *testing.T) {
	svc, counter, repo := newHighTTFTTestService(t,
		`{"enabled":true,"action":"error","threshold_count":2,"threshold_window_minutes":10,"ttft_threshold_seconds":30}`)
	account := &Account{ID: 42}

	require.False(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 60))
	require.True(t, svc.HandleHighTTFT(context.Background(), account, "gpt-5.6-sol", 60))
	require.Equal(t, []int64{42}, repo.errored)
	require.Equal(t, 1, counter.reset)
}

// 关闭 / none / 阈值边界。
func TestHighTTFTDisabledOrNoAction(t *testing.T) {
	svc, _, _ := newHighTTFTTestService(t,
		`{"enabled":false,"action":"temp_unsched","ttft_threshold_seconds":30}`)
	require.False(t, svc.HandleHighTTFT(context.Background(), &Account{ID: 1}, "m", 60))

	svc, _, _ = newHighTTFTTestService(t,
		`{"enabled":true,"action":"none","ttft_threshold_seconds":30}`)
	require.False(t, svc.HandleHighTTFT(context.Background(), &Account{ID: 1}, "m", 60))

	// 阈值边界：30s 不算高首字，31s 算。
	svc, counter, _ := newHighTTFTTestService(t,
		`{"enabled":true,"action":"error","threshold_count":1,"threshold_window_minutes":10,"ttft_threshold_seconds":30}`)
	require.False(t, svc.HandleHighTTFT(context.Background(), &Account{ID: 2}, "m", 30))
	require.True(t, svc.HandleHighTTFT(context.Background(), &Account{ID: 2}, "m", 31))
	// 正常首字重置 + 触发后清零，共两次 reset 调用。
	require.Equal(t, 2, counter.reset)
}

// 默认值归一化。
func TestHighTTFTNormalizeDefaults(t *testing.T) {
	settings := &HighTTFTSettings{}
	normalizeHighTTFTSettings(settings)
	require.Equal(t, StreamTimeoutActionTempUnsched, settings.Action)
	require.Equal(t, 5, settings.TempUnschedMinutes)
	require.Equal(t, 3, settings.ThresholdCount)
	require.Equal(t, 10, settings.ThresholdWindowMinutes)
	require.Equal(t, 30, settings.TTFTThresholdSeconds)
}
