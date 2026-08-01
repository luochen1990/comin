// 职责边界: comin desktop 子命令入口.
// 把 comin 事件流转换为桌面通知: 部署/构建的关键阶段推送一条常驻交互式通知,
// 在 confirmation 阶段(auto/manual 模式)提供"立即部署/跳过本次"双按钮, auto 模式下展示倒计时.
// without 模式走原有瞬时通知路径.
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
		"started":         "Agent 桌面通知已启动.",
		"suspended":       "Agent 已挂起.",
		"resumed":         "Agent 已恢复.",
		"eval_failed":     "评估失败.",
		"build_running":   "正在构建来自 %s/%s 的新提交.",
		"build_failed":    "构建失败.",
		"deploy_started":  "一次部署已开始.",
		"deploy_done":     "部署已完成.",
		"deploy_failed":   "部署失败.",
		"reboot_required": "需要重启机器以使本次部署生效.",
		"cancelled":       "部署已被取消.",
		// 常驻通知专用
		"deploy_now":      "立即部署",
		"skip":            "跳过本次",
		"countdown":       "剩余 %d 秒后自动放行",
		"waiting_confirm": "等待你的确认",
		"phase_building":  "正在构建新版本",
		"phase_deploying": "正在部署新版本",
	},
	"en": {
		"started":         "Agent desktop notifications started.",
		"suspended":       "The agent is suspended.",
		"resumed":         "The agent is resumed.",
		"eval_failed":     "The evaluation has failed.",
		"build_running":   "A new commit from %s/%s is building.",
		"build_failed":    "The build has failed.",
		"deploy_started":  "A deployment started.",
		"deploy_done":     "The deployment is finished.",
		"deploy_failed":   "The deployment has failed.",
		"reboot_required": "The machine needs to be rebooted to take the deployment into account.",
		"cancelled":       "The deployment has been cancelled.",
		"deploy_now":      "Deploy now",
		"skip":            "Skip this time",
		"countdown":       "Auto-confirming in %d seconds",
		"waiting_confirm": "Waiting for your confirmation",
		"phase_building":  "Building a new generation",
		"phase_deploying": "Deploying the new generation",
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
	// 持续刷新时使用的通知字段(每次刷新重新构造, 因 body 会随阶段/倒计时变化).
	summary string
	scope   string // "build" / "deploy" - 决定 Confirm 的 for 字段
	uuid    string // 待确认的 generation uuid
	// auto 模式倒计时: deadline 归零时刻, ticker 每 countdownInterval 秒刷新 body.
	deadline time.Time
	ticker   *time.Ticker
	stopCh   chan struct{} // 通知 ticker goroutine 退出
	// persistentMessage 是"基础文案"(phase_building/phase_deploying 等),
	// 刷新时 body = persistentMessage, auto 模式再追加倒计时行.
	persistentMessage string
}

const countdownInterval = 5 * time.Second

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

// close 关闭常驻通知并停止倒计时 goroutine. 幂等.
func (s *notificationState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTickerLocked()
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
				state.stopTickerLocked()
				state.currentID = 0
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

	// 启动横幅: Warn 级别, 不阻塞(沿用现有降级策略).
	if _, err := notify.SendNotification(conn, notify.Notification{
		AppName:       "comin",
		Summary:       title,
		Body:          tr("started"),
		ExpireTimeout: notify.ExpireTimeoutSetByNotificationServer,
	}); err != nil {
		logrus.Warnf("desktop: failed to send startup banner: %s", err)
	}

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

// handler 把一条事件转换为通知行为(瞬时 / 刷新常驻 / 开启常驻 / 关闭常驻).
// persistent 活跃期间, 进度类事件刷新常驻通知 body; 否则走瞬时通知.
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
			// 成功评估, 等待 build 事件再通知
		default:
			logrus.Errorf("unexpected evaluation status: %s", g.EvalStatus)
		}
	case *protobuf.Event_BuildStartedType:
		g := v.BuildStartedType.Generation
		if g.BuildReason != builder.BuildReasonNeedBuild {
			break
		}
		git := getGitFromGeneration(g)
		msg := tr("build_running", git.SelectedRemoteName, git.SelectedBranchName)
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.currentID == 0 {
			// 无活跃常驻通知(without 模式): 走瞬时路径
			sendTransient(state, msg)
			return
		}
		// 常驻通知活跃: 刷新 body 展示构建阶段
		state.scope = "build"
		state.uuid = g.Uuid
		state.persistentMessage = msg
		state.showOrUpdateLocked(msg, persistentActions())
	case *protobuf.Event_BuildFinishedType:
		g := v.BuildFinishedType.Generation
		switch g.BuildStatus {
		case "failed":
			sendTransient(state, tr("build_failed"))
		case "built":
			// 构建完成, 等待 confirmation/deploy 事件
		default:
			logrus.Errorf("unexpected build status: %s", g.BuildStatus)
		}
	case *protobuf.Event_DeploymentStartedType:
		msg := tr("deploy_started")
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.currentID == 0 {
			// 无活跃常驻通知(without 模式): 走瞬时路径
			sendTransient(state, msg)
			return
		}
		// 常驻通知活跃(confirmation 刚结束): 刷新为部署阶段, 去掉按钮
		state.persistentMessage = tr("phase_deploying")
		state.stopTickerLocked()
		state.showOrUpdateLocked(state.persistentMessage, nil)
	case *protobuf.Event_DeploymentFinishedType:
		d := v.DeploymentFinishedType.Deployment
		var msg string
		switch d.Status {
		case "done":
			msg = tr("deploy_done")
		case "failed":
			msg = tr("deploy_failed")
		default:
			logrus.Errorf("unexpected deployment status: %s", d.Status)
		}
		if msg != "" {
			// 部署结束, 常驻通知使命完成, 关闭之; 然后发瞬时结果通知.
			state.close()
			sendTransient(state, msg)
		}
	case *protobuf.Event_RebootRequired_:
		sendTransient(state, tr("reboot_required"))
	// 高频/内部事件: 静默(空 case 避免 default 分支错误日志刷屏)
	case *protobuf.Event_ManagerState_:
	case *protobuf.Event_Fetched_:
	case *protobuf.Event_Log_:
	// --- confirmation 生命周期: 决定是否开启/关闭常驻通知 ---
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

// handleConfirmationSubmitted 处理 confirmation 提交事件, 决定是否开启常驻通知.
//   - without: 立即放行, 不弹常驻通知(走瞬时路径).
//   - auto:    弹常驻通知 + 双按钮 + 倒计时.
//   - manual:  弹常驻通知 + 双按钮(无倒计时).
func handleConfirmationSubmitted(cs *protobuf.Event_ConfirmationSubmitted, createdAt *timestamppb.Timestamp, state *notificationState, c *client.Client) {
	switch cs.Mode {
	case "without":
		// 立即放行, 无需用户介入.
		return
	case "auto", "manual":
		// 进入常驻通知流程.
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
	body := tr("phase_building")
	if cs.Mode == "manual" {
		body = tr("waiting_confirm")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
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
