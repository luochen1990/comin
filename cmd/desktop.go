// 职责边界: comin desktop 子命令入口.
// 把一次部署周期(build→confirm→deploy→finished)的事件流转换为一条常驻通知, body 随阶段推进
// 原地刷新(replaces_id); confirmation 阶段在 auto/manual 模式下追加双按钮 + 倒计时.
// 部署外的独立事件(suspend/resume/reboot)走瞬时通知.
//
// body 结构 (自顶而下):
//
//	<commit subject> (<short id>)   ← commit header, 由 BuildStarted 提取, 全周期常驻
//	<阶段文案>                       ← persistentMessage (building/waiting/deploying/done/failed)
//	剩余 N 秒后自动放行              ← 倒计时行 (仅 auto 模式 confirmation 阶段)
//
// 生命周期(场景 A: BuildConfirmer=without, DeployConfirmer=auto):
//
//	BuildStarted        → 开常驻, body="<commit header>\n正在构建 origin/main", 无按钮
//	BuildFinished(built)→ 静默(等 confirm)
//	ConfirmationSubmitted(auto) → 刷新常驻, 追加按钮 + 倒计时行
//	ConfirmationConfirmed       → 停 ticker, 通知保留
//	DeploymentStarted  → 刷新常驻, body="<commit header>\n正在部署", 去按钮
//	DeploymentFinished → 刷新常驻, body="<commit header>\n部署完成", 去按钮; 由 doneTimer 延迟关闭
//	下一周期 BuildStarted → replaces_id 复用同一条通知(或上一条已被 doneTimer 关闭则新开)
//
// auto-skip (reboot_policy=skip 的 needs-reboot generation): ConfirmationSubmitted
// 携带 mode="auto-skip", 倒计时文案为 "自动跳过"; 归零时主进程发 ConfirmationExpired
// (而非 Cancelled), desktop 把 body 切到 "等待你的确认" 并保留按钮 — 通知常驻成为部署入口.
package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/esiqveland/notify"
	"github.com/godbus/dbus/v5"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/nlewo/comin/pkg/client"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- 通知 action key 常量 ---
//
// 这些字符串是 FreeDesktop notification action 协议的 key, 跨进程 (desktop client ↔ 通知 daemon)
// 传递, 改变需要两端同步. 抽为常量便于编译期拼写检查.
const (
	actionDeploy = "deploy" // deploy 通知的 "立即部署" 按钮
	actionCancel = "cancel" // deploy 通知的 "跳过本次" 按钮
	actionReboot = "reboot" // reboot 通知的 "立即重启" 按钮
	actionSkip   = "skip"   // reboot 通知的 "暂不重启" 按钮
)

// --- i18n (轻量, 按 LANG 环境变量路由) ---

// translations: locale -> key -> 模板字符串.
// 新增语言只需往这张表加条目, 翻译查找逻辑无需改动.
var translations = map[string]map[string]string{
	"zh_CN": {
		"suspended":        "Agent 已挂起.",
		"resumed":          "Agent 已恢复.",
		"eval_failed":      "评估失败.",
		"build_failed":     "构建失败.",
		"reboot_required":  "需要重启机器以使本次部署生效.",
		"cancelled":        "部署已被取消.",
		"reboot_cancelled": "已推迟重启.",
		// 常驻通知阶段文案(同一通知 body 随阶段刷新)
		"phase_building":  "正在构建来自 %s/%s 的新提交.",
		"phase_waiting":   "构建完成, 等待确认.",
		"phase_deploying": "正在部署新版本.",
		"phase_done":      "部署已完成.",
		"phase_failed":    "部署失败.",
		// 常驻通知交互元素
		"deploy_now":      "立即部署",
		"skip":            "跳过本次",
		"countdown":       "剩余 %d 秒后自动放行",
		"countdown_skip":  "剩余 %d 秒后自动跳过 (需要重启的部署仅经手动放行)",
		"waiting_confirm": "等待你的确认",
		// reboot 交互通知文案
		"reboot_now":              "立即重启",
		"reboot_skip":             "暂不重启",
		"reboot_countdown_reboot": "剩余 %d 秒后自动重启",
		"reboot_countdown_skip":   "剩余 %d 秒后自动推迟",
		"reboot_pending_hint":     "⚠ 切换后需要重启: %s",
		"reboot_prompt_title":     "部署完成, 需要重启才能生效",
		"reboot_reason_prefix":    "需要重启的原因: ",
	},
	"en": {
		"suspended":        "The agent is suspended.",
		"resumed":          "The agent is resumed.",
		"eval_failed":      "The evaluation has failed.",
		"build_failed":     "The build has failed.",
		"reboot_required":  "The machine needs to be rebooted to take the deployment into account.",
		"cancelled":        "The deployment has been cancelled.",
		"reboot_cancelled": "Reboot postponed.",
		// Persistent notification phase strings (body refreshes as phase advances)
		"phase_building":  "A new commit from %s/%s is building.",
		"phase_waiting":   "Build finished, awaiting confirmation.",
		"phase_deploying": "Deploying the new generation.",
		"phase_done":      "The deployment is finished.",
		"phase_failed":    "The deployment has failed.",
		// Persistent notification interactive elements
		"deploy_now":      "Deploy now",
		"skip":            "Skip this time",
		"countdown":       "Auto-confirming in %d seconds",
		"countdown_skip":  "Auto-skipping in %d seconds (reboot-required deploys need manual approval)",
		"waiting_confirm": "Waiting for your confirmation",
		// reboot interactive notification strings
		"reboot_now":              "Reboot now",
		"reboot_skip":             "Skip for now",
		"reboot_countdown_reboot": "Auto-rebooting in %d seconds",
		"reboot_countdown_skip":   "Auto-skipping in %d seconds",
		"reboot_pending_hint":     "⚠ Reboot required after switch: %s",
		"reboot_prompt_title":     "Deployment finished, reboot required to take effect",
		"reboot_reason_prefix":    "Reboot reasons: ",
	},
}

// currentLocale 解析 LANG 环境变量(如 "zh_CN.UTF-8" -> "zh_CN"), 取不到则 fallback "en".
// systemd user service 通常会从 PAM 继承 LANG.
func currentLocale() string {
	lang := os.Getenv("LANG")
	if lang == "" || lang == "C" || lang == "POSIX" {
		return "en"
	}
	// "zh_CN.UTF-8" -> "zh_CN"
	if idx := strings.IndexByte(lang, '.'); idx > 0 {
		lang = lang[:idx]
	}
	if _, ok := translations[lang]; ok {
		return lang
	}
	return "en"
}

// tr 按当前 locale 查翻译键, 找不到则回退 en, 仍找不到则原样返回 key.
// args 用于 Sprintf 格式化(如倒计时秒数).
func tr(key string, args ...interface{}) string {
	locale := currentLocale()
	tbl, ok := translations[locale]
	if !ok {
		tbl = translations["en"]
	}
	tmpl, ok := tbl[key]
	if !ok {
		tbl = translations["en"]
		tmpl, ok = tbl[key]
		if !ok {
			return key
		}
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

// --- reboot 配置 (从环境变量读取, systemd 注入) ---

// rebootConfig 控制 needs-reboot 交互通知的行为.
// 由 NixOS module 通过 systemd 环境变量注入 (静态配置, 不走 gRPC).
type rebootConfig struct {
	// mode: "without" (瞬时通知) | "auto" (倒计时) | "manual" (等用户)
	mode string
	// autoconfirmDuration: auto 模式的倒计时秒数
	autoconfirmDuration int
	// autoconfirmAction: auto 模式归零后的动作, "reboot" 或 "skip"
	autoconfirmAction string
	// triggers: 哪些 RebootChecks 字段触发交互通知 (kebab-case), 空切片表示 Any() 触发
	triggers []string
}

// loadRebootConfig 从环境变量解析 rebootConfirmer 配置.
// 缺失字段用合理默认值 (与 NixOS module 默认值一致):
//
//	mode=auto, duration=300, action=skip, triggers=4 项硬性要求.
//
// 特殊值: COMIN_REBOOT_TRIGGERS="none" 表示显式禁用 (shouldPromptReboot 永远返回 false).
// COMIN_REBOOT_TRIGGERS="any" 或未设置 表示任意字段为 true 都触发 (默认 4 项硬性要求).
func loadRebootConfig() rebootConfig {
	cfg := rebootConfig{
		mode:                envOr("COMIN_REBOOT_MODE", "auto"),
		autoconfirmDuration: envIntOr("COMIN_REBOOT_AUTOCONFIRM_DURATION", 300),
		autoconfirmAction:   envOr("COMIN_REBOOT_AUTOCONFIRM_ACTION", "skip"),
	}
	triggersStr := os.Getenv("COMIN_REBOOT_TRIGGERS")
	switch triggersStr {
	case "", "any":
		// 未设置 或 显式 "any": 任意字段都触发 (triggers=nil 表示 Any() 触发).
		cfg.triggers = nil
	case "none":
		// 显式禁用: triggers 为空切片, shouldPromptReboot 永远返回 false.
		cfg.triggers = []string{}
	default:
		cfg.triggers = strings.Split(triggersStr, ",")
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// shouldPromptReboot 根据 rebootConfig.triggers 判断 RebootChecks 是否应触发交互通知.
// triggers 为 nil 表示 Any() 触发 (默认); 为空切片表示禁用; 否则只触发列出的字段.
func shouldPromptReboot(checks *protobuf.RebootChecks, cfg rebootConfig) bool {
	if checks == nil || checks.IsEmpty() {
		return false
	}
	if cfg.triggers == nil {
		return checks.Any()
	}
	return checks.AnyTriggered(cfg.triggers)
}

// rebootActions 返回 reboot 交互通知的双按钮 (经 i18n).
func rebootActions() []notify.Action {
	return []notify.Action{
		{Key: actionReboot, Label: tr("reboot_now")},
		{Key: actionSkip, Label: tr("reboot_skip")},
	}
}

// --- 通知状态机 ---

// notificationState 管理一次部署周期内的常驻通知生命周期.
// 并发访问: handler(主循环) 与 action 监听 goroutine 都会读写, 全部经 mu 保护.
type notificationState struct {
	mu       sync.Mutex
	notifier notify.Notifier
	conn     *dbus.Conn
	// currentID != 0 表示有一条活跃的常驻通知, 后续 SendNotification 用 ReplacesID 原地刷新.
	currentID uint32
	// cycle 是部署周期号, 每次 BuildStarted 递增. 用于 doneTimer 回调跨周期身份校验:
	// 通知 daemon 对 replaces_id 返回相同 id, 故不能仅靠 currentID 区分本周期/下一周期.
	cycle uint64
	// 持续刷新时使用的通知字段(每次刷新重新构造, 因 body 会随阶段/倒计时变化).
	summary string
	scope   string // "build" / "deploy" - 决定 Confirm 的 for 字段
	uuid    string // 待确认的 generation uuid
	// auto 模式倒计时: deadline 归零时刻, ticker 每 countdownInterval 秒刷新 body.
	// countdownKey 决定倒计时文案 ("countdown"=自动放行 / "countdown_skip"=自动跳过),
	// 每次 ConfirmationSubmitted 按事件 mode 设置, 与 deadline 同生命周期.
	deadline     time.Time
	countdownKey string
	ticker       *time.Ticker
	stopCh       chan struct{} // 通知 ticker goroutine 退出
	// doneTimer: 部署完成后延迟关闭常驻通知(让用户看到"部署完成"结果再消失).
	doneTimer *time.Timer
	// persistentMessage 是"阶段文案"(phase_building/phase_deploying 等),
	// 刷新时 body = commitHeader + persistentMessage, auto 模式再追加倒计时行.
	persistentMessage string

	// --- commit 信息 (当前部署周期对应的提交) ---
	// 由 BuildStarted 事件从 Generation.SelectedCommitMsg / SelectedCommitId 提取,
	// 在 showOrUpdateLocked/showOrUpdateRebootLocked 底层统一拼接到 body 顶部,
	// 让所有阶段的通知都能展示"这次部署的是什么", 避免频繁交织部署时无法区分.
	//
	// 生命周期: 仅在 BuildStarted 写入, 不在 closeLocked 中清除 —
	// commit 信息需跨 deploy 通知关闭存活, 供紧随其后的 reboot 通知延续展示
	// (同一部署周期: deploy done → reboot required). 新周期 BuildStarted 会覆盖.
	commitSubject string // commit message 首行 (subject)
	commitShortID string // commit id 前 8 位 (对齐 git shortlog 粒度)

	// --- reboot 交互通知相关字段 ---
	// rebootCfg: 从环境变量读取的 rebootConfirmer 配置 (静态, 启动时一次性读取).
	rebootCfg rebootConfig
	// pendingChecks: 当前 generation 的 needs-reboot 检查结果 (BuildFinished 时计算).
	// 用于在 deploy confirmation 阶段展示 "切换后需要重启" 提示.
	pendingChecks *protobuf.RebootChecks
	// rebootNotifier: 当前 reboot 交互通知的专用 notifier (与 deploy 通知独立, 避免互相 replaces_id).
	// nil 表示没有活跃的 reboot 通知.
	rebootID uint32
	// rebootTicker / rebootDeadline / rebootStopCh: reboot 通知的倒计时机制 (与 deploy 倒计时独立).
	rebootDeadline time.Time
	rebootTicker   *time.Ticker
	rebootStopCh   chan struct{}
	// rebootClientPtr: 倒计时 goroutine 自动归零时调用 Reboot RPC 用.
	// 不直接持有 client, 而是指针, 因为 client 在 main 启动后才有值.
	rebootClientPtr *client.Client
}

const countdownInterval = 5 * time.Second

// doneDisplayDuration: 部署完成后常驻通知保留展示的时间, 超过后自动关闭.
const doneDisplayDuration = 8 * time.Second

// shortIDLen: commit short id 截取长度 (对齐 git shortlog 默认粒度).
const shortIDLen = 8

// commitSubjectFromMsg 从完整 commit message 中提取 subject (首行).
// 空消息返回空串.
func commitSubjectFromMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if idx := strings.IndexByte(msg, '\n'); idx > 0 {
		return msg[:idx]
	}
	return msg
}

// shortID 从完整 commit id 中截取前 shortIDLen 位.
func shortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}

// withCommitHeader 在 body 顶部拼接 commit header (subject + short id).
// 无 subject 时原样返回 body (避免引入空行). 调用者必须持有 s.mu.
func (s *notificationState) withCommitHeader(body string) string {
	if s.commitSubject == "" {
		return body
	}
	header := s.commitSubject
	if s.commitShortID != "" {
		header = fmt.Sprintf("%s (%s)", header, s.commitShortID)
	}
	return header + "\n" + body
}

// showOrUpdate 发送/刷新常驻通知. actions 为 nil 时不带按钮(纯进度展示).
// body 顶部自动拼接当前周期的 commit header (subject + short id).
// 调用者必须持有 s.mu.
func (s *notificationState) showOrUpdateLocked(body string, actions []notify.Action) {
	if s.notifier == nil {
		return
	}
	n := notify.Notification{
		AppName:       "comin",
		ReplacesID:    s.currentID,
		Summary:       s.summary,
		Body:          s.withCommitHeader(body),
		Actions:       actions,
		ExpireTimeout: notify.ExpireTimeoutNever, // 永不超时
	}
	id, err := s.notifier.SendNotification(n)
	if err != nil {
		logrus.Errorf("desktop: send/refresh persistent notification failed: %s", err)
		return
	}
	s.currentID = id
}

// close 关闭常驻通知并停止所有后台 goroutine(倒计时 + 延迟关闭). 幂等.
func (s *notificationState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopBackgroundTimersLocked()
	s.closeLocked()
	s.stopRebootTimersLocked()
	s.closeRebootLocked()
}

// closeLocked 关闭当前常驻通知并重置状态字段. 必须持有 s.mu.
// 不触碰 ticker/doneTimer —— 由调用方按场景决定是否先 stop(见 close / doneTimer 回调).
func (s *notificationState) closeLocked() {
	if s.currentID != 0 && s.notifier != nil {
		if _, err := s.notifier.CloseNotification(s.currentID); err != nil {
			logrus.Debugf("desktop: close notification %d failed: %s", s.currentID, err)
		}
	}
	s.currentID = 0
	s.uuid = ""
	s.scope = ""
	s.persistentMessage = ""
	// 注: 不清除 commitSubject/commitShortID — commit 信息需跨 deploy 通知关闭存活,
	// 供紧随其后的 reboot 通知延续展示 (字段生命周期见 notificationState 注释).
}

// stopBackgroundTimersLocked 停止所有后台定时器(倒计时 + 延迟关闭).
// 阶段切换/通知关闭前调用, 必须持有 s.mu.
func (s *notificationState) stopBackgroundTimersLocked() {
	s.stopTickerLocked()
	s.stopDoneTimerLocked()
}

// stopTickerLocked 停止倒计时 goroutine, 必须持有 s.mu.
func (s *notificationState) stopTickerLocked() {
	if s.ticker != nil {
		s.ticker.Stop()
		select {
		case <-s.stopCh: // 已关闭
		default:
			close(s.stopCh)
		}
		s.ticker = nil
		s.stopCh = nil
	}
}

// stopDoneTimerLocked 停止延迟关闭 timer, 必须持有 s.mu.
func (s *notificationState) stopDoneTimerLocked() {
	if s.doneTimer != nil {
		s.doneTimer.Stop()
		s.doneTimer = nil
	}
}

// --- reboot 通知的 helper methods ---

// showOrUpdateRebootLocked 发送/刷新 reboot 常驻通知. actions 为 nil 时不带按钮.
// body 顶部自动拼接当前周期的 commit header (subject + short id).
// 调用者必须持有 s.mu.
func (s *notificationState) showOrUpdateRebootLocked(body string, actions []notify.Action) {
	if s.notifier == nil {
		return
	}
	n := notify.Notification{
		AppName:       "comin",
		ReplacesID:    s.rebootID,
		Summary:       s.summary,
		Body:          s.withCommitHeader(body),
		Actions:       actions,
		ExpireTimeout: notify.ExpireTimeoutNever, // 永不超时
	}
	id, err := s.notifier.SendNotification(n)
	if err != nil {
		logrus.Errorf("desktop: send/refresh reboot notification failed: %s", err)
		return
	}
	s.rebootID = id
}

// closeRebootLocked 关闭当前 reboot 通知并重置 ID. 必须持有 s.mu.
func (s *notificationState) closeRebootLocked() {
	if s.rebootID != 0 && s.notifier != nil {
		if _, err := s.notifier.CloseNotification(s.rebootID); err != nil {
			logrus.Debugf("desktop: close reboot notification %d failed: %s", s.rebootID, err)
		}
	}
	s.rebootID = 0
}

// stopRebootTimersLocked 停止 reboot 倒计时 goroutine, 必须持有 s.mu.
func (s *notificationState) stopRebootTimersLocked() {
	if s.rebootTicker != nil {
		s.rebootTicker.Stop()
		select {
		case <-s.rebootStopCh:
		default:
			close(s.rebootStopCh)
		}
		s.rebootTicker = nil
		s.rebootStopCh = nil
	}
}

// startRebootCountdownLocked 启动 reboot auto 模式倒计时. 必须持有 s.mu.
// cfg.autoconfirmAction 决定归零后的动作 ("reboot" / "skip") 及倒计时文案.
func (s *notificationState) startRebootCountdownLocked(cfg rebootConfig) {
	s.stopRebootTimersLocked()
	s.rebootTicker = time.NewTicker(countdownInterval)
	s.rebootStopCh = make(chan struct{})
	ticker := s.rebootTicker
	stopCh := s.rebootStopCh
	deadline := s.rebootDeadline
	action := cfg.autoconfirmAction
	clientPtr := s.rebootClientPtr
	state := s
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				state.mu.Lock()
				if state.rebootID == 0 {
					state.mu.Unlock()
					return
				}
				remaining := int(time.Until(deadline).Seconds())
				if remaining <= 0 {
					// 归零: 执行 autoconfirm_action.
					body := state.rebootCountdownBody(0, action)
					state.showOrUpdateRebootLocked(body, rebootActions())
					state.stopRebootTimersLocked()
					// skip 动作需显式关闭通知 (reboot 动作由 systemctl 实际重启时 daemon 自动关闭).
					if action == "skip" {
						state.closeRebootLocked()
					}
					state.mu.Unlock()
					// reboot 动作: 在 goroutine 中调 RPC (不在持锁状态, 避免死锁).
					// clientPtr 永远非 nil (主流程初始化时赋值), 但可能指向零值 Client{} (stream 未建立);
					// 与 makeOnAction 一致用值比较判断 client 是否就绪.
					if action == "reboot" && *clientPtr != (client.Client{}) {
						logrus.Infof("desktop: reboot autoconfirm expired, triggering reboot")
						if err := clientPtr.Reboot(); err != nil {
							logrus.Errorf("desktop: auto-reboot RPC failed: %s", err)
						}
					}
					return
				}
				body := state.rebootCountdownBody(remaining, action)
				state.showOrUpdateRebootLocked(body, rebootActions())
				state.mu.Unlock()
			}
		}
	}()
}

// rebootCountdownBody 构造 reboot 倒计时通知的 body 文案. 必须持有 s.mu (因读 persistentMessage).
func (s *notificationState) rebootCountdownBody(remaining int, action string) string {
	body := tr("reboot_prompt_title")
	if s.persistentMessage != "" {
		body = s.persistentMessage
	}
	var countdownKey string
	if action == "reboot" {
		countdownKey = "reboot_countdown_reboot"
	} else {
		countdownKey = "reboot_countdown_skip"
	}
	return body + "\n" + tr(countdownKey, remaining)
}

// startCountdownLocked 启动 auto 模式倒计时刷新 goroutine. 必须持有 s.mu.
func (s *notificationState) startCountdownLocked() {
	s.stopTickerLocked()
	s.ticker = time.NewTicker(countdownInterval)
	s.stopCh = make(chan struct{})
	ticker := s.ticker
	stopCh := s.stopCh
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				s.mu.Lock()
				if s.currentID == 0 {
					s.mu.Unlock()
					return
				}
				key := s.countdownKey
				if key == "" {
					key = "countdown"
				}
				remaining := int(time.Until(s.deadline).Seconds())
				if remaining <= 0 {
					// 倒计时归零: 刷新为 0 秒并停止 ticker, 等待主进程事件关闭通知.
					// 不在此关闭通知, 因为归零 ≠ 已确认/已跳过 (主进程 timer 触发仍需几十 ms).
					body := s.persistentMessage + "\n" + tr(key, 0)
					s.showOrUpdateLocked(body, persistentActions())
					s.stopTickerLocked()
					s.mu.Unlock()
					return
				}
				body := s.persistentMessage + "\n" + tr(key, remaining)
				s.showOrUpdateLocked(body, persistentActions())
				s.mu.Unlock()
			}
		}
	}()
}

// persistentActions 返回常驻通知的双按钮 action 列表(经 i18n).
// 两个 action key 与 FreeDesktop 规范一致: actionDeploy / actionCancel.
func persistentActions() []notify.Action {
	return []notify.Action{
		{Key: actionDeploy, Label: tr("deploy_now")},
		{Key: actionCancel, Label: tr("skip")},
	}
}

// --- cobra 子命令 ---

var title string

var desktopCmd = &cobra.Command{
	Use:   "desktop",
	Short: "Send desktop notifications ",
	Args:  cobra.MinimumNArgs(0),
	Run:   runDesktop,
}

func runDesktop(cmd *cobra.Command, args []string) {
	unixSocketPath, _ := cmd.Flags().GetString("unix-socket-path")
	title, _ = cmd.Flags().GetString("title")
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// 建立 dbus 连接 + notifier(带 action 监听回调).
	conn, err := dbus.SessionBus()
	if err != nil {
		logrus.Fatalf("desktop: cannot connect to session bus: %s", err)
	}

	state := &notificationState{
		summary:       title,
		rebootCfg:     loadRebootConfig(),
		pendingChecks: &protobuf.RebootChecks{},
	}
	logrus.Infof("desktop: reboot config loaded: mode=%s duration=%d action=%s triggers=%v",
		state.rebootCfg.mode, state.rebootCfg.autoconfirmDuration,
		state.rebootCfg.autoconfirmAction, state.rebootCfg.triggers)
	// 注: notify.WithOnAction 注册的是静态回调, action 触发时 client 可能尚未/已经建立.
	// 用一个共享指针承载当前可用的 client, 回调闭包读取其最新值.
	clientPtr := &client.Client{}
	notifier, err := notify.New(
		conn,
		notify.WithOnAction(makeOnAction(state, clientPtr)),
		notify.WithOnClosed(func(sig *notify.NotificationClosedSignal) {
			// 用户手动关闭通知时, 同步清理内部状态(避免 currentID/rebootID 指向已失效通知).
			state.mu.Lock()
			switch sig.ID {
			case state.currentID:
				// 复用 closeLocked: CloseNotification 对已关闭 id 是 no-op(仅刷 Debug 日志).
				state.stopBackgroundTimersLocked()
				state.closeLocked()
			case state.rebootID:
				state.stopRebootTimersLocked()
				state.closeRebootLocked()
			}
			state.mu.Unlock()
		}),
	)
	if err != nil {
		_ = conn.Close()
		logrus.Fatalf("desktop: cannot create notifier: %s", err)
	}
	state.notifier = notifier
	state.conn = conn
	state.rebootClientPtr = clientPtr
	defer func() {
		state.close()
		_ = notifier.Close()
		_ = conn.Close()
	}()

	test, _ := cmd.Flags().GetBool("test")
	if test {
		scenario(conn, state)
		return
	}

	opts := client.ClientOpts{UnixSocketPath: unixSocketPath}
	c, err := client.New(opts)
	if err != nil {
		logrus.Fatal(err)
	}
	defer c.Close()
	*clientPtr = c // 让 action 回调能访问 client

	ctx := context.Background()
	ch := c.Stream(ctx)
	for streamer := range ch {
		if streamer.FailureMsg != "" {
			// client.Stream 内部会自动重连, 此处仅记录, 不退出
			// (退出会绕过 defer 导致常驻通知残留).
			logrus.Warnf("event stream transient failure: %s", streamer.FailureMsg)
			continue
		}
		handler(streamer.Event, state, &c)
	}
}

// makeOnAction 返回 ActionInvoked 信号回调.
// deploy: 调用 gRPC Confirm(uuid, scope); cancel: 调用 gRPC Cancel(uuid, scope).
// reboot: 调用 gRPC Reboot(); skip: 关闭 reboot 通知 (复用同一回调, 通过 action key 区分).
// (Cancel RPC 补全了 confirmation 生命周期: Confirm + Cancel 均走 gRPC.)
//
// 注: clientPtr 是共享指针, 解决"notify 回调在 client 建立前就注册"的时序问题——
// 回调触发时读取 clientPtr 最新指向, 那时 client 必然已就绪(无 client 则无事件流, 也就无 confirmation).
func makeOnAction(state *notificationState, clientPtr *client.Client) func(*notify.ActionInvokedSignal) {
	return func(sig *notify.ActionInvokedSignal) {
		state.mu.Lock()
		// 首先尝试匹配 deploy 常驻通知 (currentID).
		// 再尝试 reboot 常驻通知 (rebootID). 两者互斥 (同一时刻只有一个活跃).
		isDeployNotif := sig.ID == state.currentID && state.currentID != 0
		isRebootNotif := sig.ID == state.rebootID && state.rebootID != 0
		if !isDeployNotif && !isRebootNotif {
			state.mu.Unlock()
			return
		}
		uuid := state.uuid
		scope := state.scope
		state.mu.Unlock()

		switch sig.ActionKey {
		case actionDeploy:
			if uuid == "" {
				return
			}
			if *clientPtr == (client.Client{}) {
				logrus.Warn("desktop: client not initialized, cannot handle deploy action")
				return
			}
			logrus.Infof("desktop: user clicked deploy, confirming generation %s (%s)", uuid, scope)
			if err := clientPtr.Confirm(uuid, scope); err != nil {
				logrus.Errorf("desktop: confirm failed: %s", err)
			}
		case actionCancel:
			if uuid == "" {
				return
			}
			if *clientPtr == (client.Client{}) {
				logrus.Warn("desktop: client not initialized, cannot handle cancel action")
				return
			}
			logrus.Infof("desktop: user clicked skip, cancelling confirmation %s (%s)", uuid, scope)
			if err := clientPtr.Cancel(uuid, scope); err != nil {
				logrus.Errorf("desktop: cancel failed: %s", err)
			}
		case actionReboot:
			if *clientPtr == (client.Client{}) {
				logrus.Warn("desktop: client not initialized, cannot handle reboot action")
				return
			}
			logrus.Infof("desktop: user clicked reboot now, triggering reboot RPC")
			if err := clientPtr.Reboot(); err != nil {
				logrus.Errorf("desktop: reboot RPC failed: %s", err)
			}
			// 通知会在系统实际重启时被 daemon 自动关闭, 这里先停倒计时即可.
			state.mu.Lock()
			state.stopRebootTimersLocked()
			state.mu.Unlock()
		case actionSkip:
			// 仅 reboot 通知用 actionSkip key. 关闭通知, 停倒计时.
			logrus.Infof("desktop: user clicked skip reboot")
			state.mu.Lock()
			state.stopRebootTimersLocked()
			state.closeRebootLocked()
			state.mu.Unlock()
			// 发一个瞬时通知告知用户已推迟.
			sendTransient(state, tr("reboot_cancelled"))
		}
	}
}

// --- 事件处理 ---

// handler 把一条事件转换为通知行为.
// 常驻通知生命周期对齐到一次部署周期: BuildStarted 开启, body 随阶段原地刷新,
// DeploymentFinished 展示结果后延迟关闭. 部署外事件(suspend/resume/reboot)走瞬时通知.
func handler(event *protobuf.Event, state *notificationState, c *client.Client) {
	logrus.Debugf("received event: %s", event)
	switch v := event.Type.(type) {
	default:
		logrus.Errorf("unexpected type %T", v)
	case *protobuf.Event_Suspend_:
		sendTransient(state, tr("suspended"))
	case *protobuf.Event_Resume_:
		sendTransient(state, tr("resumed"))
	case *protobuf.Event_EvalStartedType:
		// 静默: 评估阶段太频繁, 不通知
	case *protobuf.Event_EvalFinishedType:
		g := v.EvalFinishedType.Generation
		switch g.EvalStatus {
		case "failed":
			sendTransient(state, tr("eval_failed"))
		case "evaluated":
			// 成功评估, 等待 build 事件
		default:
			logrus.Errorf("unexpected evaluation status: %s", g.EvalStatus)
		}
	case *protobuf.Event_BuildStartedType:
		g := v.BuildStartedType.Generation
		if g.BuildReason != builder.BuildReasonNeedBuild {
			break
		}
		// 开启(或复用)常驻通知, 进入构建阶段. 无按钮.
		git := getGitFromGeneration(g)
		msg := tr("phase_building", git.SelectedRemoteName, git.SelectedBranchName)
		state.mu.Lock()
		defer state.mu.Unlock()
		state.cycle++                      // 新周期: 递增周期号, 使上一周期的 doneTimer 回调身份校验失效
		state.stopBackgroundTimersLocked() // 取消上一周期的延迟关闭/残留 ticker
		state.scope = ""
		state.uuid = ""
		// 提取本次部署的 commit 信息, showOrUpdateLocked 底层会拼到 body 顶部.
		state.commitSubject = commitSubjectFromMsg(git.SelectedCommitMsg)
		state.commitShortID = shortID(git.SelectedCommitId)
		state.persistentMessage = msg
		state.showOrUpdateLocked(msg, nil)
	case *protobuf.Event_BuildFinishedType:
		g := v.BuildFinishedType.Generation
		switch g.BuildStatus {
		case "failed":
			// 构建失败: 关闭常驻通知, 发瞬时提示.
			state.close()
			sendTransient(state, tr("build_failed"))
		case "built":
			// 构建完成: 在本地复算 pendingChecks (复用同一份算法, 避免 proto 改动).
			// 用于在 deploy confirmation 阶段提示用户"切换后需要重启".
			// 注意: outPath 是 nix store 路径, 全局可读, 无权限问题.
			if g.OutPath != "" {
				state.mu.Lock()
				state.pendingChecks = utils.CheckRebootLinux(g.OutPath)
				pc := state.pendingChecks
				state.mu.Unlock()
				if pc.Any() {
					logrus.Infof("desktop: build finished, pending reboot checks: %s", pc.Reason())
				}
			}
			// 等待 confirmation/deploy 事件.
			// DeployConfirmer=without: 主进程立即放行, 紧接着 DeploymentStarted 会刷新 body.
			// DeployConfirmer=auto/manual: ConfirmationSubmitted 会追加按钮/倒计时.
		default:
			logrus.Errorf("unexpected build status: %s", g.BuildStatus)
		}
	case *protobuf.Event_DeploymentStartedType:
		// 刷新常驻为部署阶段, 去按钮(confirmation 已结束).
		state.mu.Lock()
		defer state.mu.Unlock()
		state.stopBackgroundTimersLocked()
		state.persistentMessage = tr("phase_deploying")
		state.showOrUpdateLocked(state.persistentMessage, nil)
	case *protobuf.Event_DeploymentFinishedType:
		d := v.DeploymentFinishedType.Deployment
		var msg string
		switch d.Status {
		case "done":
			msg = tr("phase_done")
		case "failed":
			msg = tr("phase_failed")
		default:
			logrus.Errorf("unexpected deployment status: %s", d.Status)
		}
		if msg == "" {
			break
		}
		// 部署结束: 在原常驻通知上更新结果(去按钮), 由 doneTimer 延迟关闭.
		// 不发额外瞬时通知 —— 常驻通知本身就展示了结果.
		state.mu.Lock()
		defer state.mu.Unlock()
		state.stopBackgroundTimersLocked()
		state.persistentMessage = msg
		state.showOrUpdateLocked(msg, nil)
		// 捕获周期号快照, 回调触发时校验防止跨周期误关(字段语义见 notificationState.cycle 注释).
		// doneTimer.Stop 对已启动的回调返回 false 且不中断, 必须靠身份校验兜底.
		expectedCycle := state.cycle
		state.doneTimer = time.AfterFunc(doneDisplayDuration, func() {
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.cycle != expectedCycle {
				// 已进入下一周期(BuildStarted 递增了 cycle), 回调作废.
				return
			}
			state.closeLocked()
			state.doneTimer = nil
		})
	case *protobuf.Event_RebootRequired_:
		// needs-reboot 状态翻转通知 (manager 从 IsEmpty → Any 时发布).
		// 按 rebootConfirmer.mode 弹出交互式通知或瞬时通知.
		handleRebootRequired(state, c)
	// 高频/内部事件: 静默(空 case 避免 default 分支错误日志刷屏)
	case *protobuf.Event_ManagerState_:
	case *protobuf.Event_Fetched_:
	case *protobuf.Event_Log_:
	// --- confirmation 生命周期: 在已活跃的常驻通知上追加交互 ---
	case *protobuf.Event_ConfirmationSubmittedType:
		handleConfirmationSubmitted(v.ConfirmationSubmittedType, event.GetCreatedAt(), state, c)
	case *protobuf.Event_ConfirmationConfirmedType:
		// 已确认: 停止倒计时, 但常驻通知保留(紧接着的 DeploymentStarted 会刷新它).
		state.mu.Lock()
		state.stopTickerLocked()
		state.mu.Unlock()
	case *protobuf.Event_ConfirmationExpiredType:
		// auto-skip 倒计时归零: confirmation 未被取消, 转入 manual 等待.
		// 常驻通知保留 (按钮保留), body 切换到 "等待确认" — 用户随时可点 "立即部署".
		state.mu.Lock()
		state.stopTickerLocked()
		state.persistentMessage = tr("waiting_confirm")
		state.showOrUpdateLocked(state.persistentMessage, persistentActions())
		state.mu.Unlock()
	case *protobuf.Event_ConfirmationCancelledType:
		// 已取消: 关闭常驻通知, 发瞬时提示.
		state.close()
		sendTransient(state, tr("cancelled"))
	}
}

// handleConfirmationSubmitted 处理 confirmation 提交事件, 在已活跃的常驻通知上追加交互.
//   - without: 立即放行, 不追加按钮(常驻通知保持纯进度展示).
//   - auto:    追加双按钮 + "自动放行"倒计时行.
//   - auto-skip: 追加双按钮 + "自动跳过"倒计时行 (reboot_policy=skip 降级后的模式).
//   - manual:  追加双按钮(无倒计时).
//
// 常驻通知通常已由 BuildStarted 开启; 若未开启(BuildStarted 被跳过的边界场景)则此处兜底开启.
func handleConfirmationSubmitted(cs *protobuf.Event_ConfirmationSubmitted, createdAt *timestamppb.Timestamp, state *notificationState, c *client.Client) {
	switch cs.Mode {
	case "without":
		// 立即放行, 无需用户介入. 常驻通知保持当前 body(构建中).
		return
	case "auto", "auto-skip", "manual":
		// 进入交互式 confirmation.
	default:
		logrus.Errorf("unexpected confirmer mode: %s", cs.Mode)
		return
	}

	// auto / auto-skip 模式需要倒计时: 从事件 CreatedAt + AutoconfirmDuration 计算归零时刻.
	// ConfirmationSubmitted 事件不携带 duration, 需通过 GetManagerState 读取 deploy_confirmer.
	var deadline time.Time
	if cs.Mode != "manual" {
		duration := autoconfirmDuration(c)
		start := time.Now()
		if createdAt != nil && createdAt.AsTime().Unix() > 0 {
			start = createdAt.AsTime()
		}
		deadline = start.Add(duration)
	}

	// 确定 scope: gRPC Confirm(uuid, scope) 的 for 字段 ("build"/"deploy"/"all").
	// 已知技术债务: ConfirmationSubmitted 事件未携带 confirmer 类型, 无法区分是 BuildConfirmer
	// 还是 DeployConfirmer 触发. 默认设为 "deploy" —— 因为 comin 典型配置 BuildConfirmer=without
	// (不进入此分支), 仅 DeployConfirmer 为 auto/manual. 若用户把 BuildConfirmer 配成 auto/manual,
	// 此处的 "deploy" 会让 build 确认发到 deploy confirmer (服务端按 uuid 查不到则静默忽略).
	// 根治需要在 protobuf Event.ConfirmationSubmitted 增加 confirmer_type 字段.
	scope := "deploy"
	body := tr("phase_waiting")
	if cs.Mode == "manual" {
		body = tr("waiting_confirm")
	}

	// 若 pendingChecks 表明切换后需要 reboot, 追加提示行 (帮助用户在 confirmation 阶段做决策).
	// 仅展示 triggers 配置关心的字段 (避免 systemd-upgraded 等软信号刷屏).
	if shouldPromptReboot(state.pendingChecks, state.rebootCfg) {
		body = body + "\n" + tr("reboot_pending_hint", state.pendingChecks.Reason())
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.stopBackgroundTimersLocked() // confirmation 事件意味着新周期进行中, 取消上一周期的延迟关闭/残留 ticker
	state.scope = scope
	state.uuid = cs.Uuid
	state.persistentMessage = body
	if cs.Mode != "manual" {
		// auto → "自动放行"; auto-skip → "自动跳过" (保守策略降级模式, 文案需向用户
		// 传达"不点就不会部署").
		countdownKey := "countdown"
		if cs.Mode == "auto-skip" {
			countdownKey = "countdown_skip"
		}
		remaining := int(time.Until(deadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		state.deadline = deadline
		state.countdownKey = countdownKey
		body = body + "\n" + tr(countdownKey, remaining)
		state.showOrUpdateLocked(body, persistentActions())
		state.startCountdownLocked()
	} else {
		state.showOrUpdateLocked(body, persistentActions())
	}
}

// autoconfirmDuration 从主进程状态读取 deploy confirmer 的自动确认时长.
// 读取失败时 fallback 0(倒计时立即归零, 不影响 confirm 流程, 仅 UI 无倒计时显示).
func autoconfirmDuration(c *client.Client) time.Duration {
	if c == nil {
		return 0
	}
	st, err := c.GetManagerState()
	if err != nil || st == nil || st.DeployConfirmer == nil {
		logrus.Debugf("desktop: cannot read manager state for autoconfirm duration: %v", err)
		return 0
	}
	return time.Duration(st.DeployConfirmer.GetAutoconfirmDuration()) * time.Second
}

// handleRebootRequired 处理 needs-reboot 状态翻转事件, 按 rebootConfirmer.mode 弹通知.
//   - without: 瞬时通知 (legacy 行为, 不交互).
//   - manual:  常驻交互通知, 无倒计时, 等用户.
//   - auto:    常驻交互通知 + 倒计时, 归零按 autoconfirm_action 执行 (默认 skip).
//
// 通知 body 展示 pendingChecks.Reason() (用户配置的 triggers 字段中为 true 的项).
// "立即重启"按钮调 gRPC Reboot; "暂不重启"按钮关闭通知.
//
// 与 deploy 通知的关系: 两者互斥 (各自独立 ID), 不互相 replaces_id.
// RebootRequired 事件通常在 DeploymentFinished 之后到达 (manager 在 deploy done 时累积翻转),
// 故 deploy 通知的 doneTimer 可能正在等待关闭; 这里中断它, 转入 reboot 通知生命周期.
func handleRebootRequired(state *notificationState, c *client.Client) {
	cfg := state.rebootCfg
	if cfg.mode == "without" {
		// legacy 行为: 仅瞬时通知, 不交互.
		sendTransient(state, tr("reboot_required"))
		return
	}

	// pendingChecks 应该已经被 BuildFinished 算好. 但 RebootRequired 可能在 switch 完成
	// (DeploymentFinished) 之后到达, 此时 pendingChecks 仍是本次 generation 的检查结果
	// (manager 在 DeployDone 后才清空 pendingChecks, 而 RebootRequired 在此之前发布).
	// 复用本地 state.pendingChecks 展示即可.
	checks := state.pendingChecks
	if !shouldPromptReboot(checks, cfg) {
		// 状态翻转了但 triggers 未命中 (如仅 systemd-upgraded 变化, 用户未启用该 trigger),
		// 不弹交互通知, 仅瞬时提示.
		logrus.Debugf("desktop: reboot required but no trigger matched, sending transient only")
		sendTransient(state, tr("reboot_required"))
		return
	}

	// 构造通知 body: 显示 reboot 原因.
	body := tr("reboot_prompt_title")
	if checks != nil && !checks.IsEmpty() {
		body = body + "\n" + tr("reboot_reason_prefix") + checks.Reason()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	// 关闭 deploy 通知 (避免 deploy 通知和 reboot 通知同时并存, 造成 UX 混乱).
	// reboot 通知接管常驻通知槽位, 持续展示直到用户决策或倒计时归零.
	// 注: closeLocked 不清除 commit 字段, 故 reboot 通知自然延续展示本次部署的 commit header.
	state.stopBackgroundTimersLocked()
	state.closeLocked()
	// 重置 reboot 状态.
	state.stopRebootTimersLocked()
	state.persistentMessage = body
	state.showOrUpdateRebootLocked(body, rebootActions())

	if cfg.mode == "auto" {
		// 启动倒计时.
		state.rebootDeadline = time.Now().Add(time.Duration(cfg.autoconfirmDuration) * time.Second)
		remaining := int(time.Until(state.rebootDeadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		body = state.rebootCountdownBody(remaining, cfg.autoconfirmAction)
		state.showOrUpdateRebootLocked(body, rebootActions())
		state.startRebootCountdownLocked(cfg)
	}
	// manual 模式: 无倒计时, 通知保留直到用户点击或手动关闭.
}

// sendTransient 发送一条瞬时通知(without 模式 / 常驻关闭后的结果提示).
func sendTransient(state *notificationState, body string) {
	if state.conn == nil {
		return
	}
	_, err := notify.SendNotification(state.conn, notify.Notification{
		AppName:       "comin",
		Summary:       title,
		Body:          body,
		ExpireTimeout: notify.ExpireTimeoutSetByNotificationServer,
	})
	if err != nil {
		logrus.Errorf("desktop: send transient notification failed: %s", err)
	}
}

// --- 测试场景 (comin desktop --test) ---

func scenario(conn *dbus.Conn, state *notificationState) {
	g := protobuf.Generation{
		BuildReason: builder.BuildReasonNeedBuild,
		Source: &protobuf.Source{
			Source: &protobuf.Source_Git{
				Git: &protobuf.Git{
					SelectedRemoteName: "origin",
					SelectedBranchName: "main",
					SelectedCommitId:   "a1b2c3d4e5f6a7b8c9d0",
					SelectedCommitMsg:  "feat(network): enable DAE eBPF routing by default\n\nDetailed body line 1.\nDetailed body line 2.",
				},
			},
		},
	}
	e := protobuf.Event{Type: &protobuf.Event_BuildStartedType{BuildStartedType: &protobuf.Event_BuildStarted{Generation: &g}}}
	handler(&e, state, nil)
	time.Sleep(time.Second)

	// 模拟 BuildFinished + 本地复算 pendingChecks (模拟 kernel 变更场景)
	state.mu.Lock()
	state.pendingChecks = &protobuf.RebootChecks{
		KernelChanged: true,
		InitrdChanged: true,
	}
	state.mu.Unlock()

	// 模拟 confirmation(auto 模式, 倒计时 30s)
	cs := protobuf.Event_ConfirmationSubmitted{Mode: "auto", Uuid: "test-uuid"}
	e = protobuf.Event{
		Type:      &protobuf.Event_ConfirmationSubmittedType{ConfirmationSubmittedType: &cs},
		CreatedAt: timestamppb.Now(),
	}
	handler(&e, state, nil)
	time.Sleep(15 * time.Second)

	// 模拟 confirmation confirmed
	cc := protobuf.Event_ConfirmationConfirmed{Uuid: "test-uuid"}
	e = protobuf.Event{Type: &protobuf.Event_ConfirmationConfirmedType{ConfirmationConfirmedType: &cc}}
	handler(&e, state, nil)
	time.Sleep(time.Second)

	d := protobuf.Deployment{Status: store.StatusToString(store.Init)}
	e = protobuf.Event{Type: &protobuf.Event_DeploymentStartedType{DeploymentStartedType: &protobuf.Event_DeploymentStarted{Deployment: &d}}}
	handler(&e, state, nil)
	time.Sleep(time.Second)

	d = protobuf.Deployment{Status: store.StatusToString(store.Done)}
	e = protobuf.Event{Type: &protobuf.Event_DeploymentFinishedType{DeploymentFinishedType: &protobuf.Event_DeploymentFinished{Deployment: &d}}}
	handler(&e, state, nil)

	// 模拟 RebootRequired: 触发 reboot 交互通知 (倒计时 + 双按钮)
	time.Sleep(2 * time.Second)
	e = protobuf.Event{Type: &protobuf.Event_RebootRequired_{RebootRequired: &protobuf.Event_RebootRequired{Deployment: &d}}}
	handler(&e, state, nil)
	// 让 reboot 通知倒计时展示一会儿
	time.Sleep(20 * time.Second)
}

func init() {
	desktopCmd.Flags().StringVarP(&title, "title", "", "comin", "the notification title")
	desktopCmd.Flags().StringP("unix-socket-path", "", "/var/lib/comin/grpc.sock", "the GRPC Unix socket path")
	desktopCmd.Flags().BoolP("test", "", false, "do not get events from the agent but from predefined scenari")
	rootCmd.AddCommand(desktopCmd)
}
