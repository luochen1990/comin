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
	// Skip the generation after several seconds unless the user
	// explicitly confirms it. This is a runtime-downgraded Auto
	// (reboot_policy=skip on a needs-reboot generation): the timer
	// fires a cancel instead of a confirm.
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
				// 保守策略降级: needs-reboot generation 不自动放行 (详见 reboot_policy.go).
				// 咨询是同步回调 (store 读 + 文件比较, 毫秒级), 在事件循环内执行可接受 —
				// 与 manager.Run 循环消费 DeploymentDoneCh 的既有并发模型一致.
				// 每次都从 configuredMode 出发, 降级不跨 generation 累积.
				needsReboot := c.rebootConsult != nil && c.rebootConsult(command.uuid)
				mode, modeStr := downgradeMode(c.configuredMode, c.rebootPolicy, needsReboot)
				if mode != c.configuredMode {
					logrus.Infof("confirmer: generation %s needs reboot, downgrading mode from %s to %s",
						command.uuid, modeString(c.configuredMode), modeStr)
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
					if c.timer != nil {
						c.timer.Stop()
					}
					c.timer = time.NewTimer(time.Duration(c.state.AutoconfirmDuration) * time.Second)
					timer = c.timer.C
					c.state.AutoconfirmStarted = wrapperspb.Bool(true)
					c.state.AutoconfirmStartedAt = timestamppb.New(time.Now().UTC())
				}
				// Notify subscribers that a generation entered the confirmation flow (buffer window started / immediate / not needed)
				submittedEvent := &protobuf.Event_ConfirmationSubmitted{Mode: modeStr, Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationSubmittedType{ConfirmationSubmittedType: submittedEvent}, CreatedAt: timestamppb.New(time.Now().UTC())})
			case "confirm":
				logrus.Infof("confirmer: generation %s has been confirmed", command.uuid)
				c.state.Confirmed = command.uuid
				e := &protobuf.Event_ConfirmationConfirmed{Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationConfirmedType{ConfirmationConfirmedType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
			case "cancel":
				logrus.Infof("confirmer: confirmation of generation %s has been cancelled", command.uuid)
				c.state.Confirmed = ""
				c.state.AutoconfirmStarted = wrapperspb.Bool(false)
				if c.timer != nil {
					c.timer.Stop()
				}
				e := &protobuf.Event_ConfirmationCancelled{Uuid: command.uuid}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationCancelledType{ConfirmationCancelledType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
			}
		case <-timer:
			if Mode(c.state.Mode) == AutoSkip {
				// auto-skip: 归零跳过而非放行. 复用 cancel 语义 (ConfirmationCancelled 事件
				// 已被 desktop 消费: 关常驻通知 + 瞬时 "已取消" 提示), 不新增事件类型.
				logrus.Infof("confirmer: timer skipped generation %s (reboot_policy=skip)", c.state.Submitted)
				skipped := c.state.Submitted
				c.state.AutoconfirmStarted = wrapperspb.Bool(false)
				c.state.Confirmed = ""
				c.state.Submitted = ""
				e := &protobuf.Event_ConfirmationCancelled{Uuid: skipped}
				c.broker.Publish(&protobuf.Event{Type: &protobuf.Event_ConfirmationCancelledType{ConfirmationCancelledType: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
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
