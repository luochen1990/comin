package manager

import (
	"fmt"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Mode int64

const (
	// The user needs to confirm the generation
	Manual Mode = iota
	// Confirm after several seconds the generation. This can be
	// cancelled by the user.
	Auto
	// Immediately confirm the submitted generation
	Without
	// Skip-then-wait mode: after the autoconfirm timer expires, fall
	// back to waiting for the user instead of confirming. This is a
	// runtime-downgraded Auto (reboot_policy=skip on a needs-reboot
	// generation): the timer publishes ConfirmationExpired and the
	// confirmer keeps waiting (as Manual) for an explicit Confirm.
	AutoSkip
)

func ParseMode(s string) (Mode, error) {
	switch s {
	case "manual":
		return Manual, nil
	case "auto":
		return Auto, nil
	case "without":
		return Without, nil
	default:
		return -1, fmt.Errorf("confirmer: invalid confirmer mode: '%s'", s)
	}
}

// modeString 返回 Mode 的事件流字符串表示 (ConfirmationSubmitted.mode 字段值).
// 与 ParseMode 刻意不对称: ParseMode 是配置入口, 必须拒绝 "auto-skip"
// (运行时降级专用模式, 不可由用户配置); modeString 是事件流出口, 覆盖全部值.
func modeString(mode Mode) string {
	switch mode {
	case Manual:
		return "manual"
	case Auto:
		return "auto"
	case AutoSkip:
		return "auto-skip"
	case Without:
		return "without"
	}
	return "unknown"
}

// Confirmer allows to handle user confirmations. A generation is
// submitted to the confirmer. Once a generation has been confirmed,
// it is pushed to the submit channel.
type Confirmer struct {
	state     *protobuf.Confirmer
	confirmed chan string
	timer     *time.Timer
	// To send a command such as confirm or cancel to the confirmer
	command    chan Command
	statusResp chan *protobuf.Confirmer
	statusReq  chan struct{}

	broker *broker.Broker
	reason string
	// configuredMode 是 NewConfirmer 传入的原始模式, 每次 submit 都从它出发计算
	// 实际生效模式 (可能被 reboot_policy 按次降级). state.Mode 只反映"当前这次
	// confirmation 的生效模式"(供 status/desktop 展示), 不作为下次 submit 的基准 —
	// 否则一次降级会永久污染后续 generation 的模式.
	configuredMode Mode
	// rebootPolicy 是 needs-reboot generation 的保守策略 (仅 deploy confirmer 使用;
	// build confirmer 恒为 rebootPolicyNone). 取值见 reboot_policy.go.
	rebootPolicy string
	// rebootConsult 同步咨询 "该 generation 是否需要 reboot 用户介入".
	// 由 manager 注入 (GenerationGet + CheckReboot + NeedsRebootConfirm);
	// 为 nil 时恒返回 false (策略不生效, 不阻塞无 store 的调用方如测试).
	rebootConsult func(generationUuid string) bool
}

func NewConfirmer(broker *broker.Broker, mode Mode, duration time.Duration, reason string) *Confirmer {
	return &Confirmer{
		state: &protobuf.Confirmer{
			AutoconfirmDuration: int64(duration.Seconds()),
			Mode:                int64(mode),
		},
		configuredMode: mode,
		confirmed:      make(chan string),
		statusReq:      make(chan struct{}),
		statusResp:     make(chan *protobuf.Confirmer),
		command:        make(chan Command),
		broker:         broker,
		reason:         reason,
	}
}

// SetRebootPolicy 配置保守策略与咨询回调. 必须在 Start 前调用 (启动后只读).
// policy 非空时 consult 不能为 nil (否则策略永远不触发, 配置形同虚设).
func (c *Confirmer) SetRebootPolicy(policy string, consult func(generationUuid string) bool) error {
	policy, err := parseRebootPolicy(policy)
	if err != nil {
		return err
	}
	if policy != rebootPolicyNone && consult == nil {
		return fmt.Errorf("confirmer: reboot_policy=%s requires a non-nil consult callback", policy)
	}
	c.rebootPolicy = policy
	c.rebootConsult = consult
	return nil
}

type Command struct {
	action string
	uuid   string
}

func (c *Confirmer) status() *protobuf.Confirmer {
	c.statusReq <- struct{}{}
	return <-c.statusResp
}

func (c *Confirmer) Submit(generationUuid string) {
	c.command <- Command{
		action: "submit",
		uuid:   generationUuid,
	}
}

func (c *Confirmer) Confirm(generationUuid string) {
	c.command <- Command{
		action: "confirm",
		uuid:   generationUuid,
	}
}
func (c *Confirmer) Cancel() {
	c.command <- Command{
		action: "cancel",
	}
}

func (c *Confirmer) Start() {
	go c.start()
}

// stopTimer 停止并丢弃挂起的 autoconfirm timer (幂等).
// 除 Stop() 外还把 c.timer 置 nil: Stop 不排空已入 channel 缓冲的 tick,
// 只有置 nil (select 的 <-timer 变 nil channel 永久阻塞) 才能免疫
// "tick 已触发 + select 同轮先选了其他分支" 的 stale-tick 竞态
// (该竞态下 Auto 模式会误放行用户刚 cancel 的 generation).
// 调用方需自行把 start() 的局部变量 timer 置 nil (闭包不可达).
func (c *Confirmer) stopTimer() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func (c *Confirmer) start() {
	logrus.Infof("confirmer: starting with the autoconfirm duration: %d seconds", c.state.AutoconfirmDuration)
	var timer <-chan time.Time
	var notified bool
	for {
		select {
		case <-c.statusReq:
			c.statusResp <- proto.CloneOf(c.state)
		case command := <-c.command:
			switch command.action {
			case "submit":
				notified = false
				c.state.Submitted = command.uuid
				// 无条件停掉上一个 confirmation 残留的 timer:
				// submit 前的 confirmation 模式可能是 Auto 而本次降级为 Manual
				// (reboot_policy) 或反之 — 不停的话 stale timer 会按旧模式归零.
				c.stopTimer()
				timer = nil
				// 保守策略降级: needs-reboot generation 不自动放行 (详见 reboot_policy.go).
				// 咨询是同步回调 (store 读 + 文件比较, 毫秒级), 在事件循环内执行可接受 —
				// 与 manager.Run 循环消费 DeploymentDoneCh 的既有并发模型一致.
				// 每次都从 configuredMode 出发, 降级不跨 generation 累积.
				needsReboot := c.rebootConsult != nil && c.rebootConsult(command.uuid)
				mode := downgradeMode(c.configuredMode, c.rebootPolicy, needsReboot)
				if mode != c.configuredMode {
					logrus.Infof("confirmer: generation %s needs reboot, downgrading mode from %s to %s",
						command.uuid, modeString(c.configuredMode), modeString(mode))
				}
				// state.Mode 反映本次 confirmation 实际生效的模式 (status/desktop/CLI 展示).
				c.state.Mode = int64(mode)
				switch mode {
				case Manual:
					logrus.Infof("confirmer: generation %s has been submitted", command.uuid)
				case Without:
					logrus.Infof("confirmer: generation %s has been submitted and confirmed", command.uuid)
					c.state.Confirmed = command.uuid
				case Auto, AutoSkip:
					action := "confirmed"
					if mode == AutoSkip {
						action = "skipped"
					}
					logrus.Infof("confirmer: generation %s has been submitted and will be %s in %d seconds",
						command.uuid, action, c.state.AutoconfirmDuration)
					c.timer = time.NewTimer(time.Duration(c.state.AutoconfirmDuration) * time.Second)
					timer = c.timer.C
					c.state.AutoconfirmStarted = wrapperspb.Bool(true)
					c.state.AutoconfirmStartedAt = timestamppb.New(time.Now().UTC())
				}
				// Notify subscribers that a generation entered the confirmation flow (buffer window started / immediate / not needed)
				submittedEvent := &protobuf.Event_ConfirmationSubmitted{Mode: modeString(mode), Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationSubmittedType{ConfirmationSubmittedType: submittedEvent}, CreatedAt: timestamppb.New(time.Now().UTC())})
			case "confirm":
				logrus.Infof("confirmer: generation %s has been confirmed", command.uuid)
				// 停掉挂起的 auto/auto-skip timer: 已确认的 confirmation 不应再被
				// 归零事件干扰 (auto-skip 归零会转 manual, auto 归零会重复 confirm).
				c.stopTimer()
				timer = nil
				c.state.Confirmed = command.uuid
				e := &protobuf.Event_ConfirmationConfirmed{Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationConfirmedType{ConfirmationConfirmedType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
			case "cancel":
				// 注: cancel 不清 Submitted — 被显式跳过的 generation 仍可通过 CLI
				// `comin confirmation accept` 反悔放行 (fetcher 对相同 commit 去重,
				// 无 poll 重入路径, 这是唯一反悔入口; 见 reboot_policy.go 头注释).
				logrus.Infof("confirmer: confirmation of generation %s has been cancelled", command.uuid)
				c.state.Confirmed = ""
				c.state.AutoconfirmStarted = wrapperspb.Bool(false)
				// 停 timer + 置 nil: 防御 "归零瞬间点击跳过" 的 stale-tick 竞态
				// (Auto 模式下 ghost tick 会误放行刚被 cancel 的 generation).
				c.stopTimer()
				timer = nil
				e := &protobuf.Event_ConfirmationCancelled{Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationCancelledType{ConfirmationCancelledType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
			}
		case <-timer:
			if Mode(c.state.Mode) == AutoSkip {
				// auto-skip 归零: 转入 manual 等待 (Submitted 保留), 通知常驻成为部署入口.
				// 设计考量: fetcher 对相同 commit 去重, "跳过后等下个 poll 重进确认" 不成立 —
				// 若归零即取消, 该 generation 在进程存活期间将无恢复途径 (仅重启 comin / push 新 commit).
				// 故超时后不取消: 停倒计时, 发 mode 转换事件让 desktop 把文案切到 "等待确认",
				// 用户随时可点 "立即部署" (Confirm) 或 "跳过本次" (Cancel).
				logrus.Infof("confirmer: auto-skip expired for generation %s, waiting for manual confirmation (reboot_policy=skip)", c.state.Submitted)
				c.state.AutoconfirmStarted = wrapperspb.Bool(false)
				c.state.Mode = int64(Manual)
				e := &protobuf.Event_ConfirmationExpired{Uuid: c.state.Submitted}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationExpiredType{ConfirmationExpiredType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
				continue
			}
			logrus.Infof("confirmer: timer confirmed generation %s", c.state.Submitted)
			c.state.AutoconfirmStarted = wrapperspb.Bool(false)
			c.state.Confirmed = c.state.Submitted
		}
		if !notified && c.state.Submitted != "" && c.state.Confirmed != "" && c.state.Confirmed == c.state.Submitted {
			notified = true
			logrus.Infof("confirmer: confirmed generation %s", c.state.Confirmed)
			c.confirmed <- c.state.Confirmed
			c.state.Confirmed = ""
			c.state.Submitted = ""
			c.state.AutoconfirmStarted = wrapperspb.Bool(false)
		}
	}
}
