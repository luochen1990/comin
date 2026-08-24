// The manager is in charge of managing relationship between
// components. Basically, it receives new commits from the fetcher,
// call the builder to evaluate and build them. Finally, it submits
// these builds to the deployer.

package manager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/executor"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Manager struct {
	// The machine id of the current host. It is used to ensure
	// the optionnal machine-id found at evaluation time
	// corresponds to the machine-id of this host.
	machineId string

	// The hostname of the current host.
	Hostname string

	stateRequestCh chan struct{}
	stateResultCh  chan *protobuf.State

	// rebootStatus 是本次启动期间累积的 needs-reboot 状态 (单调累积, 仅 reboot 重置).
	// 由多次 pendingChecks 在 DeploymentFinished 时 OR-accumulate 而成.
	// 状态翻转 (从 IsEmpty → Any) 时发布 Event_RebootRequired.
	// State.need_to_reboot 字段由 rebootStatus.Any() 派生, 不独立维护.
	rebootStatus *protobuf.RebootChecks

	// pendingChecks 是当前 build 出来的 generation (未 switch 或正在 switch) 的 reboot 检查事实.
	// BuildFinished 时计算, 不累积; 每次 BuildFinished 都重新计算一个全新的.
	// 用途: 让 deploy confirmation 阶段就能展示 "切换后需要重启: <reason>";
	// 在 DeploymentFinished(done) 时累积到 rebootStatus, 然后清空.
	pendingChecks *protobuf.RebootChecks

	// rebootCh 接收来自 Reboot RPC 的请求, 由 Run 循环串行处理 (避免与状态更新并发).
	rebootCh chan struct{}

	prometheus      prometheus.Prometheus
	storage         *store.Store
	scheduler       scheduler.Scheduler
	Fetcher         *fetcher.GitFetcher
	Builder         *builder.Builder
	deployer        *deployer.Deployer
	executor        executor.Executor
	BuildConfirmer  *Confirmer
	DeployConfirmer *Confirmer

	configurationOperations ConfigurationOperations

	isSuspended bool

	broker       *broker.Broker
	brokerEvents chan *protobuf.Event
}

func New(s *store.Store,
	p prometheus.Prometheus,
	sched scheduler.Scheduler,
	fetcher *fetcher.GitFetcher,
	builder *builder.Builder,
	deployer *deployer.Deployer,
	machineId string,
	hostname string,
	executor executor.Executor,
	buildConfirmer *Confirmer,
	deployConfirmer *Confirmer,
	broker *broker.Broker,
	configurationOperations ConfigurationOperations,
) *Manager {

	m := &Manager{
		machineId: machineId,
		Hostname:  hostname,

		stateRequestCh:          make(chan struct{}),
		stateResultCh:           make(chan *protobuf.State),
		rebootCh:                make(chan struct{}, 1),
		rebootStatus:            &protobuf.RebootChecks{},
		pendingChecks:           &protobuf.RebootChecks{},
		prometheus:              p,
		storage:                 s,
		scheduler:               sched,
		Fetcher:                 fetcher,
		Builder:                 builder,
		deployer:                deployer,
		executor:                executor,
		BuildConfirmer:          buildConfirmer,
		DeployConfirmer:         deployConfirmer,
		broker:                  broker,
		configurationOperations: configurationOperations,
		brokerEvents:            broker.Subscribe(),
	}
	return m
}

func (m *Manager) GetState() *protobuf.State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m *Manager) toState() *protobuf.State {
	// 聚合后整体克隆作统一兜底: 各组件 State() 现已在源头返回快照 (store 锁内
	// CloneOf / confirmer 克隆), 此层防御未来新增组件忘记在源头快照 — State 会被
	// server goroutine 异步 marshal (Events 初始事件 / GetState RPC), 活指针在
	// marshal 撞上并发修改时产生 size mismatch / 撕裂读.
	return proto.CloneOf(&protobuf.State{
		NeedToReboot:    wrapperspb.Bool(m.rebootStatus.Any()),
		IsSuspended:     wrapperspb.Bool(m.isSuspended),
		Builder:         m.Builder.State(),
		Deployer:        m.deployer.State(),
		Fetcher:         m.Fetcher.GetState(),
		Store:           m.storage.GetState(),
		BuildConfirmer:  m.BuildConfirmer.status(),
		DeployConfirmer: m.DeployConfirmer.status(),
	})
}

// RequestReboot 由 server.Reboot RPC 触发, 异步请求 manager 执行 reboot.
// 非阻塞 (buffered chan), 重复调用自动合并 (manager 只需执行一次 reboot).
func (m *Manager) RequestReboot() {
	select {
	case m.rebootCh <- struct{}{}:
		logrus.Infof("manager: reboot requested")
	default:
		// 已有 pending reboot 请求, 忽略重复调用 (幂等).
		logrus.Debugf("manager: reboot already pending, ignoring duplicate request")
	}
}

func (m *Manager) DeploymentLatestSubmit(operation string) error {
	latest := m.storage.GetDeploymentLastest()
	if latest == nil {
		return fmt.Errorf("manager: no previous deployment")
	}
	// If no operation is provided, use default based on branch type
	if operation == "" {
		if latest.Generation.Source != nil && latest.Generation.Source.GetGit() != nil && latest.Generation.Source.GetGit().SelectedBranchIsTesting != nil && latest.Generation.Source.GetGit().SelectedBranchIsTesting.Value {
			operation = "test"
		} else {
			operation = "switch"
		}
	}
	reason := fmt.Sprintf("The latest deployment %s has been resubmitted", latest.Uuid)
	m.deployer.Submit(latest.Generation, operation, true, reason)
	return nil
}

func (m *Manager) Suspend() error {
	if m.isSuspended {
		return fmt.Errorf("the manager is already suspended")
	}
	if err := m.Builder.Suspend(); err != nil {
		return err
	}
	m.deployer.Suspend("manager has been manually suspended")
	m.isSuspended = true
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Suspend_{Suspend: &protobuf.Event_Suspend{}}, CreatedAt: timestamppb.New(time.Now().UTC())})
	return nil
}

func (m *Manager) Resume(ctx context.Context) error {
	if !m.isSuspended {
		return fmt.Errorf("the manager is not suspended")
	}
	if err := m.Builder.Resume(ctx); err != nil {
		return err
	}
	m.deployer.Resume()
	m.isSuspended = false
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Resume_{Resume: &protobuf.Event_Resume{}}, CreatedAt: timestamppb.New(time.Now().UTC())})
	return nil
}

// FetchAndBuild fetches new commits. If a new commit is available, it
// evaluates and builds the derivation. Once built, it pushes the
// generation on a channel which is consumed by the deployer.
func (m *Manager) FetchAndBuild(ctx context.Context) {
	go func() {
		for {
			select {
			case e := <-m.brokerEvents:
				if fetched := e.GetFetched(); fetched != nil {
					if !fetched.Updated {
						continue
					}
					rs := fetched.GetGitRepositoryStatus()
					if fetched.Verified {
						logrus.Infof("manager: a generation is evaluating for commit %s", rs.SelectedCommitId)
						generation := m.storage.NewGeneration(m.Builder.GetHostname(), m.Builder.GetRepositoryDir(), m.Builder.GetSystemAttr(), rs)
						err := m.Builder.Eval(ctx, &generation)
						if err != nil {
							logrus.Error(err)
						}
					} else {
						logrus.Infof("manager: the commit %s is not evaluated because it is not signed", rs.SelectedCommitId)
					}
				}
			case generationUUID := <-m.Builder.EvaluationDone:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				if generation.EvalErr != "" {
					continue
				}
				if generation.MachineId != "" && m.machineId != generation.MachineId {
					logrus.Infof("manager: the comin.machineId %s is not the host machine-id %s", generation.MachineId, m.machineId)
				} else {
					logrus.Infof("manager: the build of the generation %s is submitted", generation.Uuid)
					m.BuildConfirmer.Submit(generationUUID)
				}
			case generationUUID := <-m.BuildConfirmer.confirmed:
				m.Builder.SubmitBuild(ctx, generationUUID)

			case generationUUID := <-m.Builder.BuildDone:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				if generation.BuildErr == "" {
					git := generation.Source.GetGit()
					logrus.Infof("manager: a generation is available for deployment with commit %s", git.SelectedCommitId)
					operation := m.getOperationFromConfigurationOperations(git.SelectedRemoteName, git.SelectedBranchName)
					if !m.deployer.IsAlreadyDeployed(&generation, operation) {
						m.DeployConfirmer.Submit(generationUUID)
					}
				} else {
					logrus.Debugf("manager: the generation %s is not being deployed because it has the error: %s", generationUUID, generation.BuildErr)
				}
			case generationUUID := <-m.DeployConfirmer.confirmed:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				git := generation.Source.GetGit()
				operation := m.getOperationFromConfigurationOperations(git.SelectedRemoteName, git.SelectedBranchName)
				reason := fmt.Sprintf("The generation %s needs to be deployed", generationUUID)
				m.deployer.Submit(&generation, operation, false, reason)
			}
		}
	}()
}

// ConfigurationOperations is a map describing the operation associated
// to each remote/branch. It is a map looking such as:
// { origin: { main: switch, testing: test }, local { main: switch }
type ConfigurationOperations map[string](map[string]string)

func (m *Manager) getOperationFromConfigurationOperations(remote, branch string) (operation string) {
	operation = "test"
	branches, ok := m.configurationOperations[remote]
	if !ok {
		logrus.Errorf("manager: could not get the remote %s. Assuming 'test' operation", remote)
		return
	}
	operation, ok = branches[branch]
	if !ok {
		logrus.Errorf("manager: could not get the operation for the branch %s/%s. Assuming test operation", remote, branch)
		return
	}
	return
}

func (m *Manager) Run(ctx context.Context) {
	logrus.Infof("manager: starting with machineId=%s", m.machineId)

	// 启动时恢复 rebootStatus:
	// 用上一次成功部署的 generation 的 outPath 与当前 booted-system 比较.
	//   - 系统 reboot 后: booted-system 已更新为 lastDpl.OutPath (init 切了 generation),
	//     CheckReboot 返回空 → rebootStatus 保持空 (正确: 系统 reboot 已清偿了所有 pending 变更).
	//   - comin 进程崩溃重启 (非系统 reboot): booted-system 仍是旧的 → CheckReboot 返回差异
	//     → 恢复累积状态 (正确: 之前未 reboot 的变更仍需告知用户).
	lastDpl := m.deployer.State().Deployment
	if lastDpl != nil && lastDpl.Generation != nil && lastDpl.Generation.OutPath != "" {
		initial := m.executor.CheckReboot(lastDpl.Generation.OutPath)
		if initial.Any() {
			m.rebootStatus = initial
			logrus.Infof("manager: restored reboot status on startup: %s", initial.Reason())
		}
	}

	// 订阅 broker 事件流, 用于在 BuildFinished 时即时计算 pendingChecks
	// (判断前移: 不等 switch, build 完成就知道这次是否需要 reboot).
	subscriber := m.broker.Subscribe()

	m.FetchAndBuild(ctx)
	m.deployer.Run(ctx)

	for {
		select {
		case <-m.stateRequestCh:
			m.stateResultCh <- m.toState()

		case ev := <-subscriber:
			// 处理 BuildFinished: 计算当前 generation 的 pendingChecks.
			if bf, ok := ev.Type.(*protobuf.Event_BuildFinishedType); ok {
				g := bf.BuildFinishedType.Generation
				if g.BuildStatus == "built" && g.OutPath != "" {
					m.pendingChecks = m.executor.CheckReboot(g.OutPath)
					if m.pendingChecks.Any() {
						logrus.Infof("manager: build finished, pending reboot checks: %s", m.pendingChecks.Reason())
					}
				}
			}

		case dpl := <-m.deployer.DeploymentDoneCh:
			// 累积 pendingChecks 到 rebootStatus (单调 OR).
			// 把"当前 generation 是否需要 reboot"的事实累积到"本次启动期间是否需要 reboot".
			if dpl.Status == "done" && m.pendingChecks != nil && m.pendingChecks.Any() {
				wasEmpty := m.rebootStatus.IsEmpty()
				m.rebootStatus.Merge(m.pendingChecks)
				// 状态翻转 (从无变更 → 有变更) 时发布 Event_RebootRequired.
				// 语义改为 "状态翻转通知" 而非 "瞬时事实" (符合 rebootStatus 是持续状态的语义).
				if wasEmpty && m.rebootStatus.Any() {
					logrus.Infof("manager: reboot status flipped to true: %s", m.rebootStatus.Reason())
					e := &protobuf.Event_RebootRequired{Deployment: proto.CloneOf(dpl)}
					m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_RebootRequired_{RebootRequired: e}, CreatedAt: timestamppb.New(time.Now().UTC())})
				}
				// pendingChecks 清空, 等下一次 build.
				// 不在 deploy 失败/cancelled 时累积: pendingChecks 反映的是"这次 build 是否需要 reboot",
				// 即使部署失败, 内核/initrd 的内容差异仍然存在, 但没 switch 就不会让 rebootStatus 累积,
				// 因为用户尚未"接受"这次变更.
				m.pendingChecks = &protobuf.RebootChecks{}
			}

			if dpl.RestartComin.GetValue() {
				// TODO: stop contexts
				logrus.Infof("manager: comin needs to be restarted")
				logrus.Infof("manager: exiting comin to let the service manager restart it")
				os.Exit(0)
			}

		case <-m.rebootCh:
			// 执行 reboot. 主进程以 root 运行, 直接调 systemctl.
			// 不检查 rebootStatus: 既然用户/客户端显式请求了, 就执行 (信任客户端决策).
			logrus.Infof("manager: executing systemctl reboot")
			cmd := exec.Command("systemctl", "reboot")
			if err := cmd.Run(); err != nil {
				logrus.Errorf("manager: systemctl reboot failed: %s", err)
			}
		}
	}
}
