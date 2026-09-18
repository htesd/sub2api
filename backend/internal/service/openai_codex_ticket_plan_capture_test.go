package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func accountWithPlan(plan string) *Account {
	return &Account{Credentials: map[string]any{"plan_type": plan}}
}

// 长度由套餐决定：上游写死 292 会让 Business/Team 账号永远拿不到票。
func TestOpenAICodexTicketTargetLengthByPlan(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 292}

	cases := []struct {
		plan string
		want int
		why  string
	}{
		{"pro", openAICodexTicketLengthPro, "pro 实测 292"},
		{"self_serve_business_prolite", openAICodexTicketLengthBusiness, "Team/Business 实测 332"},
		{"self_serve_business_usage_based", openAICodexTicketLengthBusiness, "usage_based 也是 business 族"},
	}
	for _, tc := range cases {
		got := openAICodexTicketTargetLength(accountWithPlan(tc.plan), cfg)
		assert.Equal(t, tc.want, got, "%s: %s", tc.plan, tc.why)
	}
}

// 账号级 extra 覆盖优先于 plan_type 推断。
func TestOpenAICodexTicketTargetLengthExtraOverride(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 292}

	acc := accountWithPlan("pro")
	acc.Extra = map[string]any{openAICodexTicketPlanExtraKey: "team"}
	assert.Equal(t, openAICodexTicketLengthBusiness, openAICodexTicketTargetLength(acc, cfg),
		"extra 显式指定 team 时必须覆盖 plan_type=pro 的推断")

	acc2 := accountWithPlan("self_serve_business_prolite")
	acc2.Extra = map[string]any{openAICodexTicketPlanExtraKey: "pro"}
	assert.Equal(t, openAICodexTicketLengthPro, openAICodexTicketTargetLength(acc2, cfg))
}

func TestOpenAICodexTicketTargetLengthFallsBackToConfig(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 400}
	assert.Equal(t, 400, openAICodexTicketTargetLength(accountWithPlan("mystery_plan"), cfg),
		"未知套餐回落配置值，不猜")
	assert.Equal(t, 400, openAICodexTicketTargetLength(nil, cfg))

	empty := config.OpenAICodexTicketConfig{}
	assert.Equal(t, openAICodexTicketLengthPro, openAICodexTicketTargetLength(nil, empty),
		"配置为空时回落 292")
}

// free 账号实测一律被上游 400 拒绝，不该浪费探测。
func TestOpenAICodexTicketPlanEligibility(t *testing.T) {
	assert.False(t, openAICodexTicketPlanEligible(accountWithPlan("free")), "free 无 astra/sol 权限")
	assert.True(t, openAICodexTicketPlanEligible(accountWithPlan("pro")))
	assert.True(t, openAICodexTicketPlanEligible(accountWithPlan("self_serve_business_prolite")))
	assert.True(t, openAICodexTicketPlanEligible(accountWithPlan("")), "未知套餐不预先排除")
}

func TestNormalizeOpenAICodexTicketPlan(t *testing.T) {
	assert.Equal(t, "pro", normalizeOpenAICodexTicketPlan(accountWithPlan("Pro")))
	assert.Equal(t, "business", normalizeOpenAICodexTicketPlan(accountWithPlan("self_serve_business_prolite")))
	assert.Equal(t, "business", normalizeOpenAICodexTicketPlan(accountWithPlan("team_premium")))
	assert.Equal(t, "free", normalizeOpenAICodexTicketPlan(accountWithPlan("free")))
	assert.Equal(t, "unknown", normalizeOpenAICodexTicketPlan(accountWithPlan("")))
	assert.Equal(t, "unknown", normalizeOpenAICodexTicketPlan(nil))
}

// 被动采集默认开启；主动探测循环默认关闭。
func TestOpenAICodexTicketDefaultsPreferPassiveCapture(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	assert.True(t, svc.openAICodexTicketPassiveCaptureEnabled(), "被动采集零风险，应默认开启")
	assert.False(t, svc.openAICodexTicketConfig().HarvestLoopEnabled,
		"主动探测会制造跨 ASN 关联特征，必须默认关闭")
}

func TestOpenAICodexTicketPassiveCaptureCanBeDisabled(t *testing.T) {
	off := false
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAICodexTicket.PassiveCaptureEnabled = &off
	assert.False(t, svc.openAICodexTicketPassiveCaptureEnabled())
}

// 代理池：逗号分隔解析、去重、兼容单个 URL。
func TestSplitOpenAICodexTicketHarvestProxyPool(t *testing.T) {
	assert.Equal(t, []string{"http://a:1"}, SplitOpenAICodexTicketHarvestProxyPool("http://a:1"),
		"单个 URL 必须兼容")
	assert.Equal(t,
		[]string{"http://a:1", "socks5h://b:2", "http://c:3"},
		SplitOpenAICodexTicketHarvestProxyPool(" http://a:1 , socks5h://b:2\nhttp://c:3 "))
	assert.Equal(t, []string{"http://a:1"},
		SplitOpenAICodexTicketHarvestProxyPool("http://a:1,http://a:1"), "重复项去掉")
	assert.Empty(t, SplitOpenAICodexTicketHarvestProxyPool("  ,  "))
}

// 轮换：优先未烧的出口，全烧时退回最早标记的那个而不是放弃。
func TestPickOpenAICodexTicketHarvestProxyRotatesAndAvoidsBurned(t *testing.T) {
	svc := &OpenAIGatewayService{}
	pool := []string{"http://a:1", "http://b:2", "http://c:3"}
	now := time.Now()

	seen := map[string]bool{}
	for i := 0; i < len(pool)*2; i++ {
		seen[svc.pickOpenAICodexTicketHarvestProxy(pool, now)] = true
	}
	assert.Len(t, seen, 3, "轮换必须覆盖池中全部出口")

	svc2 := &OpenAIGatewayService{}
	svc2.openAICodexTicketProxyBurn("http://a:1", 10*time.Minute, now)
	svc2.openAICodexTicketProxyBurn("http://b:2", 10*time.Minute, now)
	for i := 0; i < 5; i++ {
		assert.Equal(t, "http://c:3", svc2.pickOpenAICodexTicketHarvestProxy(pool, now),
			"只剩 c 未烧时必须固定选它")
	}
}

func TestPickOpenAICodexTicketHarvestProxyAllBurnedPicksOldest(t *testing.T) {
	svc := &OpenAIGatewayService{}
	pool := []string{"http://a:1", "http://b:2"}
	now := time.Now()
	svc.openAICodexTicketProxyBurn("http://a:1", 10*time.Minute, now.Add(-9*time.Minute))
	svc.openAICodexTicketProxyBurn("http://b:2", 10*time.Minute, now.Add(-1*time.Minute))

	assert.Equal(t, "http://a:1", svc.pickOpenAICodexTicketHarvestProxy(pool, now),
		"全部在冷却时选最早被标记的（冷却最久，最可能已恢复），而不是放弃打票")
}

func TestOpenAICodexTicketProxyRecoverClearsBurn(t *testing.T) {
	svc := &OpenAIGatewayService{}
	now := time.Now()
	svc.openAICodexTicketProxyBurn("http://a:1", time.Hour, now)
	until, _ := svc.openAICodexTicketProxyBurnState("http://a:1")
	assert.False(t, until.IsZero())

	svc.openAICodexTicketProxyRecover("http://a:1")
	until2, _ := svc.openAICodexTicketProxyBurnState("http://a:1")
	assert.True(t, until2.IsZero(), "打出合格票后必须立即解除标记")
}

func TestValidateOpenAICodexTicketHarvestProxyPool(t *testing.T) {
	assert.NoError(t, ValidateOpenAICodexTicketHarvestProxyPool(""))
	assert.NoError(t, ValidateOpenAICodexTicketHarvestProxyPool("http://a.example:1080,socks5h://b.example:1080"))
	err := ValidateOpenAICodexTicketHarvestProxyPool("http://a.example:1080,ftp://bad.example:21")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "#2", "错误要指出是第几个，便于定位")
	assert.NotContains(t, err.Error(), "bad.example", "错误信息不得回显凭据所在的原串")
}

func TestMaskOpenAICodexTicketHarvestProxyPoolHidesPasswords(t *testing.T) {
	masked := MaskOpenAICodexTicketHarvestProxyPool("http://u:secret@a.example:1080,http://u2:secret2@b.example:1080")
	assert.NotContains(t, masked, "secret")
	assert.NotContains(t, masked, "secret2")
	assert.Contains(t, masked, "a.example")
	assert.Contains(t, masked, "b.example")
}

// 活跃窗口：只给近期跑过流量的账号打票。
func TestOpenAICodexTicketAccountRecentlyUsed(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{HarvestActiveWindowSeconds: 3600}
	now := time.Now()

	recent := now.Add(-10 * time.Minute)
	stale := now.Add(-10 * time.Hour)

	assert.True(t, openAICodexTicketAccountRecentlyUsed(&Account{LastUsedAt: &recent}, cfg, now))
	assert.False(t, openAICodexTicketAccountRecentlyUsed(&Account{LastUsedAt: &stale}, cfg, now))
	assert.False(t, openAICodexTicketAccountRecentlyUsed(&Account{}, cfg, now),
		"从未使用过的账号不打票")

	unlimited := config.OpenAICodexTicketConfig{HarvestActiveWindowSeconds: -1}
	assert.True(t, openAICodexTicketAccountRecentlyUsed(&Account{LastUsedAt: &stale}, unlimited, now),
		"窗口 <=0 表示不限制")
}

// —— 以下用例对应 kimi 审查发现的问题，逐条锁住回归 ——

// P1：候选必须绑定铸造账号，否则 failover 换号后 A 号的 state 会落到 B 号名下。
func TestOpenAICodexTicketCandidateBoundToAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	accA := &Account{ID: 11}
	accB := &Account{ID: 22}
	state := makeTurnState(openAICodexTicketFernetVersion, time.Now(), 210)

	stageOpenAICodexTicketCandidate(c, accA, state)
	assert.Equal(t, state, stagedOpenAICodexTicketCandidate(c, accA), "同号必须取到")
	assert.Empty(t, stagedOpenAICodexTicketCandidate(c, accB),
		"换号后不得取到上一个账号铸造的 state")
}

// P1：空 state 必须清掉上一个 attempt 的候选。
func TestOpenAICodexTicketCandidateClearedOnEmptyState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	acc := &Account{ID: 11}

	stageOpenAICodexTicketCandidate(c, acc, makeTurnState(openAICodexTicketFernetVersion, time.Now(), 210))
	require.NotEmpty(t, stagedOpenAICodexTicketCandidate(c, acc))

	stageOpenAICodexTicketCandidate(c, acc, "")
	assert.Empty(t, stagedOpenAICodexTicketCandidate(c, acc),
		"本 attempt 无 turn-state 时必须清除候选，否则会复用上一个 attempt 的")
}

// P2：代理池的掩码回显判定——任一成员是掩码就保留旧值。
func TestIsMaskedOpenAICodexTicketHarvestProxyPool(t *testing.T) {
	assert.True(t, IsMaskedOpenAICodexTicketHarvestProxyPool(""), "空串保留旧值")
	assert.True(t, IsMaskedOpenAICodexTicketHarvestProxyPool("http://u:***@a.example:1080"))
	assert.True(t,
		IsMaskedOpenAICodexTicketHarvestProxyPool("http://u:real@a.example:1080,http://u:***@b.example:1080"),
		"半真半掩无法可靠合并，必须整体保留旧值")
	assert.False(t,
		IsMaskedOpenAICodexTicketHarvestProxyPool("http://u:real@a.example:1080,http://u2:real2@b.example:1080"),
		"全是真凭据时才写入")
}

// —— 以下用例对应 gpt-5.6-sol 审查发现的问题 ——

// 被动采集是唯一来源时 fail_closed 会形成自举死锁：没票→拒发→没有成功回合→
// 永远没票。所以它只在主动探测开启时生效，且与账号/transport 无关。
func TestOpenAICodexTicketFailClosedRequiresIndependentSource(t *testing.T) {
	svc := &OpenAIGatewayService{}

	passiveOnly := config.OpenAICodexTicketConfig{FailClosed: true, HarvestLoopEnabled: false}
	assert.False(t, svc.openAICodexTicketFailClosedActive(passiveOnly),
		"只有被动采集时 fail_closed 必须失效，否则自举死锁")

	withActive := config.OpenAICodexTicketConfig{FailClosed: true, HarvestLoopEnabled: true}
	assert.True(t, svc.openAICodexTicketFailClosedActive(withActive),
		"主动探测能打破自举，此时 fail_closed 生效")

	assert.False(t, svc.openAICodexTicketFailClosedActive(
		config.OpenAICodexTicketConfig{FailClosed: false, HarvestLoopEnabled: true}))
}

// 烧号判定必须与长度判定同口径：账号级 codex_ticket_plan 覆盖也算套餐已知。
func TestOpenAICodexTicketPlanKnownMatchesTargetLengthResolution(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 292}

	// plan_type 未知但 extra 显式标了 team：长度判定按 332，烧号判定也必须认。
	acc := accountWithPlan("brand_new_plan")
	acc.Extra = map[string]any{openAICodexTicketPlanExtraKey: "team"}
	assert.Equal(t, openAICodexTicketLengthBusiness, openAICodexTicketTargetLength(acc, cfg))
	assert.True(t, openAICodexTicketPlanKnown(acc),
		"extra 覆盖让长度有确定依据，两套口径不得撕裂")

	assert.True(t, openAICodexTicketPlanKnown(accountWithPlan("pro")))
	assert.True(t, openAICodexTicketPlanKnown(accountWithPlan("self_serve_business_prolite")))
	assert.False(t, openAICodexTicketPlanKnown(accountWithPlan("brand_new_plan")),
		"纯未知套餐走回落长度，不能据此烧代理")
	assert.False(t, openAICodexTicketPlanKnown(nil))
}

// 长度不符只在套餐已知时才能归因到出口 IP；未知套餐走回落值，
// 一个合法的 332 会把好代理误判成污染。
func TestOpenAICodexTicketProxyBurnRequiresKnownPlan(t *testing.T) {
	assert.Equal(t, "unknown", normalizeOpenAICodexTicketPlan(accountWithPlan("brand_new_plan")),
		"未知套餐必须可识别，烧号判定要据此跳过")
	assert.NotEqual(t, "unknown", normalizeOpenAICodexTicketPlan(accountWithPlan("pro")))
	assert.NotEqual(t, "unknown", normalizeOpenAICodexTicketPlan(accountWithPlan("self_serve_business_prolite")))
}
