package service

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTurnState 造一个头部合法的 turn-state：版本字节 + 大端签发时间戳 + 填充。
func makeTurnState(version byte, issuedAt time.Time, payloadLen int) string {
	raw := make([]byte, 0, 9+payloadLen)
	raw = append(raw, version)
	ts := make([]byte, 8)
	binary.BigEndian.PutUint64(ts, uint64(issuedAt.Unix()))
	raw = append(raw, ts...)
	raw = append(raw, make([]byte, payloadLen)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestDecodeOpenAICodexTicketIssuedAtReadsFernetHeader(t *testing.T) {
	issuedAt := time.Unix(1_760_000_000, 0).UTC()
	state := makeTurnState(openAICodexTicketFernetVersion, issuedAt, 210)

	version, got, err := decodeOpenAICodexTicketIssuedAt(state)
	require.NoError(t, err)
	assert.Equal(t, byte(openAICodexTicketFernetVersion), version)
	assert.True(t, got.Equal(issuedAt), "issued_at 应与写入的时间戳一致: got=%s want=%s", got, issuedAt)
}

func TestDecodeOpenAICodexTicketIssuedAtRejectsShortAndGarbage(t *testing.T) {
	_, _, err := decodeOpenAICodexTicketIssuedAt(base64.RawURLEncoding.EncodeToString([]byte{0x80, 0x00}))
	assert.Error(t, err, "不足 9 字节必须报错")

	_, _, err = decodeOpenAICodexTicketIssuedAt("!!!not-base64!!!")
	assert.Error(t, err, "非 base64 必须报错")

	_, _, err = decodeOpenAICodexTicketIssuedAt("")
	assert.Error(t, err, "空串必须报错")
}

func TestResolveOpenAICodexTicketIssuedAtFallsBackToNow(t *testing.T) {
	now := time.Now()
	ttl := time.Hour

	t.Run("头部合法则采用签发时间", func(t *testing.T) {
		issuedAt := now.Add(-30 * time.Second).Truncate(time.Second)
		got := resolveOpenAICodexTicketIssuedAt(makeTurnState(openAICodexTicketFernetVersion, issuedAt, 210), now, ttl, 1, "gpt-6-astra")
		assert.True(t, got.Equal(issuedAt), "应采用票据自述时间: got=%s want=%s", got, issuedAt)
	})

	t.Run("版本字节不符回退到now", func(t *testing.T) {
		issuedAt := now.Add(-30 * time.Second).Truncate(time.Second)
		got := resolveOpenAICodexTicketIssuedAt(makeTurnState(0x7f, issuedAt, 210), now, ttl, 1, "gpt-6-astra")
		assert.True(t, got.Equal(now), "版本不符必须回退 now")
	})

	t.Run("签发时间早于一个TTL之前回退到now", func(t *testing.T) {
		stale := now.Add(-2 * time.Hour).Truncate(time.Second)
		got := resolveOpenAICodexTicketIssuedAt(makeTurnState(openAICodexTicketFernetVersion, stale, 210), now, ttl, 1, "gpt-6-astra")
		assert.True(t, got.Equal(now), "过旧时间戳必须回退 now，否则票据一出生就过期")
	})

	t.Run("签发时间显著超前回退到now", func(t *testing.T) {
		future := now.Add(30 * time.Minute).Truncate(time.Second)
		got := resolveOpenAICodexTicketIssuedAt(makeTurnState(openAICodexTicketFernetVersion, future, 210), now, ttl, 1, "gpt-6-astra")
		assert.True(t, got.Equal(now), "超前时间戳必须回退 now，否则票据永不过期")
	})

	t.Run("解析失败回退到now", func(t *testing.T) {
		got := resolveOpenAICodexTicketIssuedAt("gAAAAA-not-really-base64!!", now, ttl, 1, "gpt-6-astra")
		assert.True(t, got.Equal(now))
	})
}

// 有效期必须锚定签发时间：捕获延迟不该被白送成额外寿命。
func TestOpenAICodexTicketExpiryAnchoredOnIssuedAt(t *testing.T) {
	ttl := time.Hour
	issuedAt := time.Now().Add(-90 * time.Second).Truncate(time.Second)
	capturedAt := time.Now()

	ticket := &openAICodexTicket{
		State:      makeTurnState(openAICodexTicketFernetVersion, issuedAt, 210),
		Length:     292,
		CapturedAt: capturedAt,
		IssuedAt:   issuedAt,
		ExpiresAt:  issuedAt.Add(ttl),
	}

	assert.True(t, ticket.ExpiresAt.Before(capturedAt.Add(ttl)),
		"按签发时间计算的过期点必须早于按捕获时间计算的，差值即探测往返延迟")
}

func newTicketBackoffConfig() config.OpenAICodexTicketConfig {
	return config.OpenAICodexTicketConfig{
		HarvestBackoffBaseSeconds: 30,
		HarvestBackoffMaxSeconds:  900,
	}
}

func TestOpenAICodexTicketBackoffEscalatesAndCaps(t *testing.T) {
	svc := &OpenAIGatewayService{}
	cfg := newTicketBackoffConfig()
	key := openAICodexTicketKey(42, "gpt-6-astra")
	now := time.Now()

	assert.True(t, svc.openAICodexTicketProbeAllowed(key, now), "没有失败记录时必须允许探测")

	svc.noteOpenAICodexTicketProbeFailure(key, cfg, now)
	assert.False(t, svc.openAICodexTicketProbeAllowed(key, now), "刚失败必须处于退避窗口内")
	assert.True(t, svc.openAICodexTicketProbeAllowed(key, now.Add(40*time.Second)),
		"base=30s 加最多 20%% 抖动，40s 后必须放行")

	// 连续失败足够多次后应该顶到 max 而不是无限翻倍。
	for i := 0; i < 20; i++ {
		svc.noteOpenAICodexTicketProbeFailure(key, cfg, now)
	}
	assert.False(t, svc.openAICodexTicketProbeAllowed(key, now.Add(14*time.Minute)),
		"退避应已接近 15 分钟上限")
	assert.True(t, svc.openAICodexTicketProbeAllowed(key, now.Add(19*time.Minute)),
		"退避必须被 max 钳住，不能无限增长")
}

func TestOpenAICodexTicketBackoffClearedOnSuccess(t *testing.T) {
	svc := &OpenAIGatewayService{}
	cfg := newTicketBackoffConfig()
	key := openAICodexTicketKey(7, "gpt-5.6-sol")
	now := time.Now()

	svc.noteOpenAICodexTicketProbeFailure(key, cfg, now)
	require.False(t, svc.openAICodexTicketProbeAllowed(key, now))

	svc.clearOpenAICodexTicketBackoff(key)
	assert.True(t, svc.openAICodexTicketProbeAllowed(key, now), "打票成功后必须立刻解除退避")
}

func TestOpenAICodexTicketBackoffIsPerAccountModel(t *testing.T) {
	svc := &OpenAIGatewayService{}
	cfg := newTicketBackoffConfig()
	now := time.Now()

	failing := openAICodexTicketKey(1, "gpt-6-astra")
	other := openAICodexTicketKey(1, "gpt-5.6-sol")
	svc.noteOpenAICodexTicketProbeFailure(failing, cfg, now)

	assert.False(t, svc.openAICodexTicketProbeAllowed(failing, now))
	assert.True(t, svc.openAICodexTicketProbeAllowed(other, now),
		"退避必须按 (账号, 模型) 隔离，一个模型失败不能拖累另一个")
}

func TestOpenAICodexTicketConfigDefaultsBoundHarvest(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	cfg := svc.openAICodexTicketConfig()

	assert.Equal(t, defaultOpenAICodexTicketHarvestConcurrency, cfg.HarvestConcurrency,
		"必须有并发上限，否则数千账号会无限 fan-out")
	assert.Equal(t, defaultOpenAICodexTicketBackoffBaseSeconds, cfg.HarvestBackoffBaseSeconds)
	assert.Equal(t, defaultOpenAICodexTicketBackoffMaxSeconds, cfg.HarvestBackoffMaxSeconds)
	assert.GreaterOrEqual(t, cfg.HarvestBackoffMaxSeconds, cfg.HarvestBackoffBaseSeconds)
}

func TestOpenAICodexTicketConfigClampsInvertedBackoffWindow(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{
		HarvestBackoffBaseSeconds: 600,
		HarvestBackoffMaxSeconds:  60,
	}
	cfg := svc.openAICodexTicketConfig()

	assert.Equal(t, 600, cfg.HarvestBackoffMaxSeconds, "max < base 时必须抬到 base，避免退避倒挂")
}
