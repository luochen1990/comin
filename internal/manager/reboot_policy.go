// 职责边界: deploy confirmation 的 needs-reboot 保守策略.
//
// 动机: switch 一个需要 reboot 才能生效的 generation 会造成"半生效"状态 —
// 用户态服务已是新版, 内核/initrd 仍是旧的. 此时若无人在场, 自动放行是危险的.
// 本文件把 "该 generation 是否需要 reboot" 的事实映射为 confirmer 行为的降级:
//
//	auto + reboot_policy=skip  → auto-skip  (倒计时归零转入 manual 等待, 仅用户点击"立即部署"才 switch)
//	auto + reboot_policy=manual→ manual     (无限等待用户确认)
//	其他组合                    → 不变
//
// 判定阈值: 复用 rebootConfirmer.triggers (SSOT) — 与 desktop 提示行
// "⚠ 切换后需要重启" 使用同一份 AnyTriggered(triggers) 语义,
// 用户在哪看到提示, 哪个策略就在哪生效, 不会出现 "desktop 有提示但策略不拦" 的分裂.
//
// 显式跳过的语义: fetcher 对相同 commit 去重, "跳过后等下个 poll 重进确认" 不可达 —
// 若归零即取消, 该 generation 在进程存活期间将无恢复途径. 故 skip 的归零动作不是取消,
// 而是 "转入 manual 等待" (Event_ConfirmationExpired): 通知常驻保留按钮, 用户随时可点
// "立即部署" (Confirm) 放行; 用户也可点 "跳过本次" (Cancel) 显式拒绝. 无人操作时系统
// 保持旧 generation 运行 (保守目标达成).
package manager

import (
	"fmt"

	"github.com/nlewo/comin/pkg/protobuf"
)

// RebootPolicy 常量: Confirmer.RebootPolicy 字段的合法取值.
const (
	// rebootPolicyNone 不干预 confirmation 流程 (默认).
	rebootPolicyNone = ""
	// rebootPolicyManual needs-reboot 时降级为 manual 模式 (无限等待).
	rebootPolicyManual = "manual"
	// rebootPolicySkip needs-reboot 时降级为 auto-skip 模式
	// (倒计时归零转入 manual 等待, 见文件头 "显式跳过的语义").
	rebootPolicySkip = "skip"
)

// parseRebootPolicy 校验并归一化配置字符串. 非法值报错 (fail fast, 避免静默吞配置).
func parseRebootPolicy(s string) (string, error) {
	switch s {
	case rebootPolicyNone, rebootPolicyManual, rebootPolicySkip:
		return s, nil
	}
	return "", fmt.Errorf("confirmer: invalid reboot_policy: '%s' (want one of '', 'manual', 'skip')", s)
}

// downgradeMode 计算当前 mode 在保守策略下的降级结果.
// 输入 mode 必须已通过 ParseMode 校验 (configuredMode).
//
//	无策略 或 非 NeedsRebootConfirm 或 mode 已是 Manual/Without → 原样返回
//	auto + policy=skip                                           → AutoSkip
//	auto + policy=manual                                         → Manual
func downgradeMode(mode Mode, rebootPolicy string, needsRebootConfirm bool) Mode {
	if !needsRebootConfirm || rebootPolicy == rebootPolicyNone || mode != Auto {
		return mode
	}
	switch rebootPolicy {
	case rebootPolicySkip:
		return AutoSkip
	case rebootPolicyManual:
		return Manual
	}
	return mode
}

// NeedsRebootConfirm 判定 generation 的 reboot 检查结果是否命中保守策略阈值.
// triggers 为 nil 表示 Any() (与 desktop shouldPromptReboot 的 "未设置" 语义一致);
// 空 (且非 nil) 切片表示显式禁用 (永不降级).
func NeedsRebootConfirm(checks *protobuf.RebootChecks, triggers []string) bool {
	if checks == nil {
		return false
	}
	if triggers == nil {
		return checks.Any()
	}
	return checks.AnyTriggered(triggers)
}
