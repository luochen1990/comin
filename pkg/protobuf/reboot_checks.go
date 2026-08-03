// 职责边界: RebootChecks 手写 Go 类型 (非 protoc 生成).
//
// 为避免引入 protoc 工具链依赖 (buildGoModule 不含), 此文件手写 RebootChecks 及其辅助方法.
// proto 文件 (services.proto) 保留为 SSOT, 后续若引入 protoc 工具链可一键生成替代本文件.
//
// 设计原则: 纯事实陈述, 每个字段对应一个独立的 reboot 检查维度 (kernel/initrd/modules/ABI/systemd).
// 不替用户判断哪些是 "require" / "nice to have"; 由 rebootConfirmer.triggers 配置触发阈值.
package protobuf

import "strings"

// RebootChecks 是一次 build 产物的 needs-reboot 检查结果, 纯事实.
// 一旦计算完毕就不可变; 每次新 build 会算出一个新的 RebootChecks.
//
// 字段语义 (详见 internal/utils/reboot.go):
//   - KernelChanged / InitrdChanged / KernelModulesChanged / SystemdABIChanged:
//     硬性 reboot 要求 (内容变更, 不重启无法生效).
//   - SystemdUpgraded:
//     软性 reboot 建议 (PID 1 跑旧版本, 可选借此机会一并 reboot).
type RebootChecks struct {
	KernelChanged        bool `json:"kernel_changed,omitempty"`
	InitrdChanged        bool `json:"initrd_changed,omitempty"`
	KernelModulesChanged bool `json:"kernel_modules_changed,omitempty"`
	SystemdABIChanged    bool `json:"systemd_abi_changed,omitempty"`
	SystemdUpgraded      bool `json:"systemd_upgraded,omitempty"`
}

// Any 表示是否有任何 reboot 相关变更 (硬性 OR 软性).
// 用于 State.need_to_reboot 派生: 只要 Any() 为 true 就视为 needs reboot.
func (c *RebootChecks) Any() bool {
	if c == nil {
		return false
	}
	return c.KernelChanged || c.InitrdChanged || c.KernelModulesChanged ||
		c.SystemdABIChanged || c.SystemdUpgraded
}

// AnyTriggered 返回是否有任何 triggers 配置命中的字段为 true.
// triggers 用 kebab-case (与 NixOS module-options.nix 的 triggers 列表约定一致, 如 "kernel-changed").
// 用于 rebootConfirmer: 只有用户配置关心的字段为 true 时, 才弹交互式通知.
func (c *RebootChecks) AnyTriggered(triggers []string) bool {
	if c == nil || len(triggers) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(triggers))
	for _, t := range triggers {
		set[t] = struct{}{}
	}
	if _, ok := set["kernel-changed"]; ok && c.KernelChanged {
		return true
	}
	if _, ok := set["initrd-changed"]; ok && c.InitrdChanged {
		return true
	}
	if _, ok := set["kernel-modules-changed"]; ok && c.KernelModulesChanged {
		return true
	}
	if _, ok := set["systemd-abi-changed"]; ok && c.SystemdABIChanged {
		return true
	}
	if _, ok := set["systemd-upgraded"]; ok && c.SystemdUpgraded {
		return true
	}
	return false
}

// Reason 返回所有为 true 的字段的人类可读描述, 用于通知展示.
// 返回空串表示无任何变更.
func (c *RebootChecks) Reason() string {
	if c == nil {
		return ""
	}
	var parts []string
	if c.KernelChanged {
		parts = append(parts, "kernel changed")
	}
	if c.InitrdChanged {
		parts = append(parts, "initrd changed")
	}
	if c.KernelModulesChanged {
		parts = append(parts, "kernel-modules changed")
	}
	if c.SystemdABIChanged {
		parts = append(parts, "systemd ABI changed")
	}
	if c.SystemdUpgraded {
		parts = append(parts, "systemd upgraded")
	}
	return strings.Join(parts, ", ")
}

// Merge 用 OR 把 other 的每个字段累积到 c (mutates c).
// 用于 RebootStatus 单调累积: 一旦某字段为 true 就不回退, 仅 reboot 重置.
// other 为 nil 时是 no-op.
func (c *RebootChecks) Merge(other *RebootChecks) {
	if c == nil || other == nil {
		return
	}
	c.KernelChanged = c.KernelChanged || other.KernelChanged
	c.InitrdChanged = c.InitrdChanged || other.InitrdChanged
	c.KernelModulesChanged = c.KernelModulesChanged || other.KernelModulesChanged
	c.SystemdABIChanged = c.SystemdABIChanged || other.SystemdABIChanged
	c.SystemdUpgraded = c.SystemdUpgraded || other.SystemdUpgraded
}

// IsEmpty 表示所有字段都为 false (零值/空状态).
func (c *RebootChecks) IsEmpty() bool {
	if c == nil {
		return true
	}
	return !c.KernelChanged && !c.InitrdChanged && !c.KernelModulesChanged &&
		!c.SystemdABIChanged && !c.SystemdUpgraded
}
