// 职责边界: needs-reboot 事实采集器.
//
// 本文件提供两类信号 (详见 docs/reboot-research.md):
//   - 硬性 reboot 要求: kernel/initrd/kernel-modules/systemd-ABI 内容变更, 不重启无法生效.
//   - 软性 reboot 建议: systemd 版本号升级, 用户可选是否借此机会一并 reboot 让新 PID 1 生效.
//
// 设计原则: 只忠实采集事实, 不替用户判断哪些是 "require" / "nice to have".
// 由 rebootConfirmer.triggers option 让用户配置自己关心的判定阈值.
//
// 算法选型: 全部用 store path / 文件内容比较, 不解析版本号.
// store path 是内容寻址哈希, 两个 path 不同意味着内容不同 (版本/编译参数/modules/config 全包).
// 这天然支持累积语义: current vs booted 的不等关系一旦成立, 持续到 reboot.
package utils

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
)

// runBootedSystem 是当前开机加载的 system generation 的固定路径 (init 固定, 重启前不变).
// 用于和 outPath/newPath 对比, 判断"开机状态"与"新 generation"的差异.
const runBootedSystem = "/run/booted-system"

// checkSpec 描述一个 reboot 检查维度的元数据, 用于表驱动检查.
// 把"字段名 / 比较方式 / 标记方法"集中表达, 新增维度只需加一行表项.
type checkSpec struct {
	name    string                         // symlink/文件名 (如 "kernel", "init-interface-version")
	symlink bool                           // true=symlinkChanged, false=fileChanged
	set     func(c *protobuf.RebootChecks) // 标记对应字段为 true
}

// rebootCheckSpecs 是所有 reboot 检查维度的声明式清单 (SSOT).
// 新增维度只需在此追加一行; rebootChecks.go 的 Reason()/AnyTriggered() 各自维护自己的视图.
var rebootCheckSpecs = []checkSpec{
	{"kernel", true, func(c *protobuf.RebootChecks) { c.KernelChanged = true }},
	{"initrd", true, func(c *protobuf.RebootChecks) { c.InitrdChanged = true }},
	{"kernel-modules", true, func(c *protobuf.RebootChecks) { c.KernelModulesChanged = true }},
	{"init-interface-version", false, func(c *protobuf.RebootChecks) { c.SystemdABIChanged = true }},
	{"systemd", true, func(c *protobuf.RebootChecks) { c.SystemdUpgraded = true }},
}

// CheckRebootLinux 比较 booted-system 与 outPath 的关键 symlink / 文件内容,
// 返回结构化的 reboot 检查结果 (纯事实, 不做聚合).
//
// outPath 通常是刚 build 出来的 system generation 的 store path
// (未必 switch, 即 BuildFinished 阶段就能调用).
func CheckRebootLinux(outPath string) *protobuf.RebootChecks {
	c := &protobuf.RebootChecks{}
	for _, spec := range rebootCheckSpecs {
		var changed bool
		var reason string
		if spec.symlink {
			changed, reason = symlinkChanged(runBootedSystem, outPath, spec.name)
		} else {
			changed, reason = fileChanged(runBootedSystem, outPath, spec.name)
		}
		if changed {
			spec.set(c)
			logrus.Infof("reboot-check: %s changed (%s)", spec.name, reason)
		}
	}
	return c
}

// symlinkChanged 比较 base/<name> 与 newBase/<name> 两个 symlink 的最终目标 (readlink -f 等价).
// 任一边读 link 失败 (文件不存在/不是 symlink) 视为"未变更" (避免首次启动或非 NixOS 环境误报).
// 返回 (changed, reason): reason 用于日志, 形如 "abc...-linux-6.7.6 -> xyz...-linux-6.8.0".
func symlinkChanged(base, newBase, name string) (changed bool, reason string) {
	oldTarget, err := readlinkTarget(filepath.Join(base, name))
	if err != nil {
		// 读不到不报错: booted-system 可能没有该字段 (老 NixOS / 非 NixOS).
		return false, ""
	}
	newTarget, err := readlinkTarget(filepath.Join(newBase, name))
	if err != nil {
		return false, ""
	}
	if oldTarget != newTarget {
		return true, shortStorePath(oldTarget) + " -> " + shortStorePath(newTarget)
	}
	return false, ""
}

// fileChanged 比较 base/<name> 与 newBase/<name> 两个普通文件的内容.
// 用于 init-interface-version 这种非 symlink 的纯文本文件.
// 任一边读取失败视为"未变更" (避免首次启动或非 NixOS 环境误报).
func fileChanged(base, newBase, name string) (changed bool, reason string) {
	oldContent, err := os.ReadFile(filepath.Join(base, name))
	if err != nil {
		return false, ""
	}
	newContent, err := os.ReadFile(filepath.Join(newBase, name))
	if err != nil {
		return false, ""
	}
	if string(oldContent) != string(newContent) {
		return true, strings.TrimSpace(string(oldContent)) + " -> " + strings.TrimSpace(string(newContent))
	}
	return false, ""
}

// readlinkTarget 返回 symlink 的直接目标 (单层 Readlink, 非递归).
// NixOS 的 /run/{booted,current}-system/<name> 都是单层 symlink → store path, 不需要递归解析.
// 路径不是 symlink 时返回错误 (调用方据此区分 symlink 字段 vs 普通文件字段).
func readlinkTarget(p string) (string, error) {
	target, err := os.Readlink(p)
	if err != nil {
		return "", err
	}
	// symlink target 通常是绝对 store path, 但也可能是相对的, 统一解析为绝对.
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	return target, nil
}

// shortStorePath 把 /nix/store/<32-char-hash>-<name> 截短为 <hash-prefix>-<name>,
// 仅用于日志/通知展示, 避免长哈希刷屏. 非标准 store path 原样返回.
func shortStorePath(p string) string {
	const storeDir = "/nix/store/"
	if !strings.HasPrefix(p, storeDir) {
		return p
	}
	name := strings.TrimPrefix(p, storeDir)
	// store path basename 形如 <32 chars hash>-<rest>, 截取 hash 前 8 字符.
	if idx := strings.IndexByte(name, '-'); idx > 0 {
		hashPrefix := name[:min(8, idx)]
		return hashPrefix + "-" + name[idx+1:]
	}
	return name
}
