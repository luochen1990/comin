// 职责边界: comin desktop 子命令入口.
// 把一次部署周期(build→confirm→deploy→finished)的事件流转换为一条常驻通知, body 随阶段推进
// 原地刷新(replaces_id); confirmation 阶段在 auto/manual 模式下追加双按钮 + 倒计时.
// 部署外的独立事件(suspend/resume/reboot)走瞬时通知.
//
// 生命周期(场景 A: BuildConfirmer=without, DeployConfirmer=auto):
//
//	BuildStarted        → 开常驻, body="正在构建 origin/main", 无按钮
//	BuildFinished(built)→ 静默(等 confirm)
//	ConfirmationSubmitted(auto) → 刷新常驻, 追加按钮 + 倒计时行
//	ConfirmationConfirmed       → 停 ticker, 通知保留
//	DeploymentStarted  → 刷新常驻, body="正在部署", 去按钮
//	DeploymentFinished → 刷新常驻, body="部署完成", 去按钮; 由 doneTimer 延迟关闭
//	下一周期 BuildStarted → replaces_id 复用同一条通知(或上一条已被 doneTimer 关闭则新开)
package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/esiqveland/notify"
	"github.com/godbus/dbus/v5"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/pkg/client"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- i18n (轻量, 按 LANG 环境变量路由) ---

// translations: locale -> key -> 模板字符串.
// 新增语言只需往这张表加条目, 翻译查找逻辑无需改动.
var translations = map[string]map[string]string{
	"zh_CN": {
		"suspended":       "Agent 已挂起.",
		"resumed":         "Agent 已恢复.",
		"eval_failed":     "评估失败.",
		"build_failed":    "构建失败.",
		"reboot_required": "需要重启机器以使本次部署生效.",
		"cancelled":       "部署已被取消.",
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
		"waiting_confirm": "等待你的确认",
	},
	"en": {
		"suspended":       "The agent is suspended.",
		"resumed":         "The agent is resumed.",
		"eval_failed":     "The evaluation has failed.",
		"build_failed":    "The build has failed.",
		"reboot_required": "The machine needs to be rebooted to take the deployment into account.",
		"cancelled":       "The deployment has been cancelled.",
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
		"waiting_confirm": "Waiting for your confirmation",
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
	deadline time.Time
	ticker   *time.Ticker
	stopCh   chan struct{} // 通知 ticker goroutine 退出
	// doneTimer: 部署完成后延迟关闭常驻通知(让用户看到"部署完成"结果再消失).
	doneTimer *time.Timer
	// persistentMessage 是"基础文案"(phase_building/phase_deploying 等),
	// 刷新时 body = persistentMessage, auto 模式再追加倒计时行.
	persistentMessage string
}

const countdownInterval = 5 * time.Second

// doneDisplayDuration: 部署完成后常驻通知保留展示的时间, 超过后自动关闭.
const doneDisplayDuration = 8 * time.Second

// showOrUpdate 发送/刷新常驻通知. actions 为 nil 时不带按钮(纯进度展示).
// 调用者必须持有 s.mu.
func (s *notificationState) showOrUpdateLocked(body string, actions []notify.Action) {
	if s.notifier == nil {
		return
	}
	n := notify.Notification{
		AppName:       "comin",
		ReplacesID:    s.currentID,
		Summary:       s.summary,
		Body:          body,
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
				remaining := int(time.Until(s.deadline).Seconds())
				if remaining <= 0 {
					// 倒计时归零: 刷新为 0 秒并停止 ticker, 等待主进程 ConfirmationConfirmed 事件关闭通知.
					// 不在此关闭通知, 因为归零 ≠ 已确认 (主进程 timer 触发仍需几十 ms).
					body := s.persistentMessage + "\n" + tr("countdown", 0)
					s.showOrUpdateLocked(body, persistentActions())
					s.stopTickerLocked()
					s.mu.Unlock()
					return
				}
				body := s.persistentMessage + "\n" + tr("countdown", remaining)
				s.showOrUpdateLocked(body, persistentActions())
				s.mu.Unlock()
			}
		}
	}()
}

// persistentActions 返回常驻通知的双按钮 action 列表(经 i18n).
// 两个 action key 与 FreeDesktop 规范一致: "deploy" / "cancel".
func persistentActions() []notify.Action {
	return []notify.Action{
		{Key: "deploy", Label: tr("deploy_now")},
		{Key: "cancel", Label: tr("skip")},
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

	state := &notificationState{summary: title}
	// 注: notify.WithOnAction 注册的是静态回调, action 触发时 client 可能尚未/已经建立.
	// 用一个共享指针承载当前可用的 client, 回调闭包读取其最新值.
	clientPtr := &client.Client{}
	notifier, err := notify.New(
		conn,
		notify.WithOnAction(makeOnAction(state, clientPtr)),
		notify.WithOnClosed(func(sig *notify.NotificationClosedSignal) {
			// 用户手动关闭通知时, 同步清理内部状态(避免 currentID 指向已失效通知).
			state.mu.Lock()
			if sig.ID == state.currentID {
				// 复用 closeLocked: CloseNotification 对已关闭 id 是 no-op(仅刷 Debug 日志).
				state.stopBackgroundTimersLocked()
				state.closeLocked()
			}
			state.mu.Unlock()
		}),
	)
	if err != nil {
		conn.Close()
		logrus.Fatalf("desktop: cannot create notifier: %s", err)
	}
	state.notifier = notifier
	state.conn = conn
	defer func() {
		state.close()
		notifier.Close()
		conn.Close()
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
// (Cancel RPC 补全了 confirmation 生命周期: Confirm + Cancel 均走 gRPC.)
//
// 注: clientPtr 是共享指针, 解决"notify 回调在 client 建立前就注册"的时序问题——
// 回调触发时读取 clientPtr 最新指向, 那时 client 必然已就绪(无 client 则无事件流, 也就无 confirmation).
func makeOnAction(state *notificationState, clientPtr *client.Client) func(*notify.ActionInvokedSignal) {
	return func(sig *notify.ActionInvokedSignal) {
		state.mu.Lock()
		// 只响应当前常驻通知的 action, 忽略其他来源( notifier 会收到总线上所有通知的信号).
		if sig.ID != state.currentID {
			state.mu.Unlock()
			return
		}
		uuid := state.uuid
		scope := state.scope
		state.mu.Unlock()
		if uuid == "" {
			return
		}
		if *clientPtr == (client.Client{}) {
			logrus.Warn("desktop: client not initialized, cannot handle action")
			return
		}
		switch sig.ActionKey {
		case "deploy":
			logrus.Infof("desktop: user clicked deploy, confirming generation %s (%s)", uuid, scope)
			if err := clientPtr.Confirm(uuid, scope); err != nil {
				logrus.Errorf("desktop: confirm failed: %s", err)
			}
		case "cancel":
			logrus.Infof("desktop: user clicked skip, cancelling confirmation %s (%s)", uuid, scope)
			if err := clientPtr.Cancel(uuid, scope); err != nil {
				logrus.Errorf("desktop: cancel failed: %s", err)
			}
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
			// 构建完成, 等待 confirmation/deploy 事件.
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
		sendTransient(state, tr("reboot_required"))
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
	case *protobuf.Event_ConfirmationCancelledType:
		// 已取消: 关闭常驻通知, 发瞬时提示.
		state.close()
		sendTransient(state, tr("cancelled"))
	}
}

// handleConfirmationSubmitted 处理 confirmation 提交事件, 在已活跃的常驻通知上追加交互.
//   - without: 立即放行, 不追加按钮(常驻通知保持纯进度展示).
//   - auto:    追加双按钮 + 倒计时行.
//   - manual:  追加双按钮(无倒计时).
//
// 常驻通知通常已由 BuildStarted 开启; 若未开启(BuildStarted 被跳过的边界场景)则此处兜底开启.
func handleConfirmationSubmitted(cs *protobuf.Event_ConfirmationSubmitted, createdAt *timestamppb.Timestamp, state *notificationState, c *client.Client) {
	switch cs.Mode {
	case "without":
		// 立即放行, 无需用户介入. 常驻通知保持当前 body(构建中).
		return
	case "auto", "manual":
		// 进入交互式 confirmation.
	default:
		logrus.Errorf("unexpected confirmer mode: %s", cs.Mode)
		return
	}

	// auto 模式需要倒计时: 从事件 CreatedAt + AutoconfirmDuration 计算归零时刻.
	// ConfirmationSubmitted 事件不携带 duration, 需通过 GetManagerState 读取 deploy_confirmer.
	var deadline time.Time
	if cs.Mode == "auto" {
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

	state.mu.Lock()
	defer state.mu.Unlock()
	state.stopBackgroundTimersLocked() // confirmation 事件意味着新周期进行中, 取消上一周期的延迟关闭/残留 ticker
	state.scope = scope
	state.uuid = cs.Uuid
	state.persistentMessage = body
	if cs.Mode == "auto" {
		remaining := int(time.Until(deadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		state.deadline = deadline
		body = body + "\n" + tr("countdown", remaining)
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
				},
			},
		},
	}
	e := protobuf.Event{Type: &protobuf.Event_BuildStartedType{BuildStartedType: &protobuf.Event_BuildStarted{Generation: &g}}}
	handler(&e, state, nil)
	time.Sleep(time.Second)

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

	time.Sleep(time.Second)
	e = protobuf.Event{Type: &protobuf.Event_RebootRequired_{RebootRequired: &protobuf.Event_RebootRequired{Deployment: &d}}}
	handler(&e, state, nil)
}

func init() {
	desktopCmd.Flags().StringVarP(&title, "title", "", "comin", "the notification title")
	desktopCmd.Flags().StringP("unix-socket-path", "", "/var/lib/comin/grpc.sock", "the GRPC Unix socket path")
	desktopCmd.Flags().BoolP("test", "", false, "do not get events from the agent but from predefined scenari")
	rootCmd.AddCommand(desktopCmd)
}
