package manager

import (
	"testing"
	"time"

	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/stretchr/testify/assert"
)

// downgradeMode 是保守策略的核心映射: 表驱动覆盖全部组合.
func TestDowngradeMode(t *testing.T) {
	cases := []struct {
		name       string
		mode       Mode
		policy     string
		needs      bool
		wantMode   Mode
		wantString string
	}{
		// 无策略: 一切照旧
		{"auto + no policy + needs", Auto, rebootPolicyNone, true, Auto, "auto"},
		{"auto + no policy + no needs", Auto, rebootPolicyNone, false, Auto, "auto"},
		// policy=skip: 仅 needs-reboot 的 auto 降级为 auto-skip
		{"auto + skip + needs", Auto, rebootPolicySkip, true, AutoSkip, "auto-skip"},
		{"auto + skip + no needs", Auto, rebootPolicySkip, false, Auto, "auto"},
		// policy=manual: 仅 needs-reboot 的 auto 降级为 manual
		{"auto + manual + needs", Auto, rebootPolicyManual, true, Manual, "manual"},
		{"auto + manual + no needs", Auto, rebootPolicyManual, false, Auto, "auto"},
		// 非 auto 模式不降级 (manual 本来就等用户; without 是显式选择立即放行)
		{"manual + skip + needs", Manual, rebootPolicySkip, true, Manual, "manual"},
		{"without + skip + needs", Without, rebootPolicySkip, true, Without, "without"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMode, gotString := downgradeMode(tc.mode, tc.policy, tc.needs)
			assert.Equal(t, tc.wantMode, gotMode)
			assert.Equal(t, tc.wantString, gotString)
		})
	}
}

func TestParseRebootPolicy(t *testing.T) {
	for _, valid := range []string{"", "manual", "skip"} {
		got, err := parseRebootPolicy(valid)
		assert.NoError(t, err)
		assert.Equal(t, valid, got)
	}
	_, err := parseRebootPolicy("deploy")
	assert.Error(t, err)
}

// NeedsRebootConfirm 的阈值语义与 desktop shouldPromptReboot 对齐 (SSOT):
// nil triggers = Any(); 空切片 = 显式禁用; 其他 = AnyTriggered.
func TestNeedsRebootConfirm(t *testing.T) {
	hardChecks := &protobuf.RebootChecks{KernelChanged: true}
	softChecks := &protobuf.RebootChecks{SystemdUpgraded: true}

	assert.False(t, NeedsRebootConfirm(nil, nil))
	assert.False(t, NeedsRebootConfirm(&protobuf.RebootChecks{}, nil))
	assert.True(t, NeedsRebootConfirm(hardChecks, nil))                          // nil → Any()
	assert.True(t, NeedsRebootConfirm(softChecks, nil))                          // Any() 含软信号
	assert.False(t, NeedsRebootConfirm(hardChecks, []string{}))                  // 空切片 = 禁用
	assert.False(t, NeedsRebootConfirm(softChecks, []string{"kernel-changed"}))  // 默认 triggers 不含软信号
	assert.True(t, NeedsRebootConfirm(softChecks, []string{"systemd-upgraded"})) // 显式加软信号
	assert.True(t, NeedsRebootConfirm(hardChecks, []string{"initrd-changed", "kernel-changed"}))
}

func TestSetRebootPolicy(t *testing.T) {
	c := NewConfirmer(nil, Auto, time.Second, "deploy")

	assert.NoError(t, c.SetRebootPolicy("", nil)) // 空策略无需回调
	assert.NoError(t, c.SetRebootPolicy("skip", func(string) bool { return true }))
	err := c.SetRebootPolicy("reboot", nil)
	assert.Error(t, err)
	err = c.SetRebootPolicy("skip", nil)
	assert.Error(t, err) // 策略非空必须提供回调
}
