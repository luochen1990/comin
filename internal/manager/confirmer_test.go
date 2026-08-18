package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/stretchr/testify/assert"
)

func TestConfirmerSubmit(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Manual, time.Second, "")
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 3*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
	}, 1*time.Second, 100*time.Millisecond)
}

func TestConfirmerManual(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Manual, time.Second, "")
	go func() {
		<-c.confirmed
	}()
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 3*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.Equal(ct, "", c.status().Confirmed)
	}, 1*time.Second, 100*time.Millisecond)
}

func TestConfirmerWithout(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Without, 0, "")
	var expectedUuid atomic.Bool
	go func() {
		t := <-c.confirmed
		if t == "uuid1" {
			expectedUuid.Store(true)
		}
	}()
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 3*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, expectedUuid.Load())
	}, 1*time.Second, 100*time.Millisecond)
}

func TestConfirmerAuto(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 2*time.Second, "")
	var expectedUuid atomic.Bool
	go func() {
		t := <-c.confirmed
		if t == "uuid1" {
			expectedUuid.Store(true)
		}
	}()
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 1*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.Equal(ct, "", c.status().Confirmed)
	}, 1*time.Second, 100*time.Millisecond)

	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, expectedUuid.Load())
	}, 3*time.Second, 100*time.Millisecond)
}

func TestConfirmerAutoCancel(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 2*time.Second, "")
	go func() {
		<-c.confirmed
	}()
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 1*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.True(ct, c.status().AutoconfirmStarted.GetValue())
	}, 1*time.Second, 100*time.Millisecond)

	c.Cancel()
	assert.Never(t, func() bool {
		return c.status().Confirmed == "uuid1"
	}, 3*time.Second, 100*time.Millisecond)
}

func TestConfirmerResubmit(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 3*time.Second, "")
	var expectedUuid atomic.Bool
	go func() {
		t := <-c.confirmed
		if t == "uuid2" {
			expectedUuid.Store(true)
		}
	}()
	c.Start()
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "", c.status().Submitted)
	}, 1*time.Second, 100*time.Millisecond)

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.True(ct, c.status().AutoconfirmStarted.GetValue())
	}, 1*time.Second, 100*time.Millisecond)
	assert.Never(t, func() bool {
		return c.status().Confirmed == "uuid1"
	}, 1*time.Second, 100*time.Millisecond)

	c.Submit("uuid2")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, c.status().AutoconfirmStarted.GetValue())
	}, 1*time.Second, 100*time.Millisecond)
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, expectedUuid.Load())
	}, 4*time.Second, 100*time.Millisecond)
}

func TestConfirmerConfirmBeforeSubmit(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 3*time.Second, "")
	var expectedUuid atomic.Bool
	go func() {
		t := <-c.confirmed
		if t == "uuid1" {
			expectedUuid.Store(true)
		}
	}()
	c.Start()
	c.Confirm("uuid1")
	assert.Never(t, func() bool {
		return expectedUuid.Load()
	}, 1*time.Second, 100*time.Millisecond)
	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, expectedUuid.Load())
	}, 4*time.Second, 100*time.Millisecond)
}

// --- reboot_policy 行为测试 ---

// auto-skip: needs-reboot generation 在倒计时归零后转入 manual 等待 (不 confirm),
// Submitted 保留 — 用户显式 Confirm 是唯一放行途径.
func TestConfirmerAutoSkipOnReboot(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 2*time.Second, "deploy")
	assert.NoError(t, c.SetRebootPolicy("skip", func(string) bool { return true }))
	confirmed := make(chan string, 1)
	go func() {
		confirmed <- <-c.confirmed
	}()
	c.Start()

	c.Submit("uuid1")
	// 降级生效: 模式变为 AutoSkip (state 对外可见).
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.Equal(ct, int64(AutoSkip), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)

	// 归零后: 不放行, 转入 manual 等待 (Submitted 保留, Mode=Manual, 倒计时结束).
	assert.Never(t, func() bool {
		return len(confirmed) != 0
	}, 1*time.Second, 100*time.Millisecond)
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid1", c.status().Submitted)
		assert.Equal(ct, int64(Manual), c.status().Mode)
		assert.False(ct, c.status().AutoconfirmStarted.GetValue())
	}, 4*time.Second, 100*time.Millisecond)
	select {
	case <-confirmed:
		t.Fatal("auto-skip must not confirm")
	default:
	}
}

// auto-skip 窗口内用户显式确认: 照常放行 (点击 "立即部署" 是唯一部署途径),
// 且归零后的 stale timer 不会干扰 (无幽灵事件).
func TestConfirmerAutoSkipUserOverrides(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 2*time.Second, "deploy")
	assert.NoError(t, c.SetRebootPolicy("skip", func(string) bool { return true }))
	var expectedUuid atomic.Bool
	go func() {
		t := <-c.confirmed
		if t == "uuid1" {
			expectedUuid.Store(true)
		}
	}()
	c.Start()

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(AutoSkip), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)

	c.Confirm("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.True(ct, expectedUuid.Load())
	}, 1*time.Second, 100*time.Millisecond)
	// 等过原 timer 归零时刻: confirm 后状态保持干净, 不出现幽灵 cancel/expire.
	time.Sleep(3 * time.Second)
	assert.Equal(t, "", c.status().Submitted)
	assert.Equal(t, "", c.status().Confirmed)
}

// 降级不跨 generation 累积: needs-reboot 的 uuid1 归零转等待后,
// 无需 reboot 的 uuid2 提交时仍走普通 auto 放行 (且 uuid1 的等待被替代).
func TestConfirmerDowngradeNotSticky(t *testing.T) {
	bk := broker.New()
	bk.Start()
	needs := false
	c := NewConfirmer(bk, Auto, 2*time.Second, "deploy")
	assert.NoError(t, c.SetRebootPolicy("skip", func(string) bool { return needs }))
	var got atomic.Value
	go func() {
		for uuid := range c.confirmed {
			got.Store(uuid)
		}
	}()
	c.Start()

	needs = true
	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(AutoSkip), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)
	// 等 uuid1 归零转 manual 等待.
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(Manual), c.status().Mode)
	}, 4*time.Second, 100*time.Millisecond)

	needs = false
	c.Submit("uuid2")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(Auto), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid2", got.Load())
	}, 4*time.Second, 100*time.Millisecond)
}

// 交错场景 (Review 发现的回归): gen1 (Auto, 无需 reboot) 提交后 60s 窗口内
// gen2 (needs-reboot, 降级 Manual) 到达 — gen1 的 stale timer 不得误放行 gen2.
func TestConfirmerAutoThenManualDowngradeNoStaleTimerLeak(t *testing.T) {
	bk := broker.New()
	bk.Start()
	needs := false
	c := NewConfirmer(bk, Auto, 2*time.Second, "deploy")
	assert.NoError(t, c.SetRebootPolicy("manual", func(string) bool { return needs }))
	go func() {
		for range c.confirmed {
		}
	}()
	c.Start()

	needs = false
	c.Submit("uuid1") // 普通 auto, 2s timer 武装
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(Auto), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)

	needs = true
	c.Submit("uuid2") // 降级 manual (替代 uuid1; submit 无条件停旧 timer)
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, "uuid2", c.status().Submitted)
		assert.Equal(ct, int64(Manual), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)

	// 越过 uuid1 的归零时刻: uuid2 不得被 stale timer 自动放行.
	assert.Never(t, func() bool {
		return c.status().Confirmed != ""
	}, 3*time.Second, 100*time.Millisecond)
	assert.Equal(t, "uuid2", c.status().Submitted)
}

// reboot_policy=manual: needs-reboot generation 降级为无限等待, 不放行.
func TestConfirmerManualDowngradeOnReboot(t *testing.T) {
	bk := broker.New()
	bk.Start()
	c := NewConfirmer(bk, Auto, 1*time.Second, "deploy")
	assert.NoError(t, c.SetRebootPolicy("manual", func(string) bool { return true }))
	c.Start()

	c.Submit("uuid1")
	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.Equal(ct, int64(Manual), c.status().Mode)
	}, 1*time.Second, 100*time.Millisecond)
	// 超过 autoconfirm duration 仍不放行 (manual 无 timer).
	assert.Never(t, func() bool {
		return c.status().Confirmed != ""
	}, 2*time.Second, 100*time.Millisecond)
}
