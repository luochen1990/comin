package deployer

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dustin/go-humanize"
	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/types"
	"github.com/nlewo/comin/internal/utils"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	ReasonDeploymentBuilder = "builder"
	ReasonDeploymentManual  = "manual"
)

type DeployFunc func(context.Context, string, string, []string, io.WriteCloser, io.WriteCloser) (bool, string, error)

type Deployer struct {
	GenerationCh     chan *protobuf.Generation
	deployerFunc     DeployFunc
	DeploymentDoneCh chan *protobuf.Deployment
	mu               sync.Mutex
	// deploymentUuid/previousDeploymentUuid (d.mu 保护): 只记 uuid, 不持对象指针 —
	// deployment 对象本体归 store 所有 (DeploymentStarted/Finished 持 s.mu 修改),
	// deployer 侧读取一律经 store.GetDeploymentSnapshot 锁内快照, 避免跨锁共享
	// 可变 proto 的数据竞争.
	// 已知良性边界: 若 previous deployment 是 failed 且被 retention 逐出
	// (anyCapacity=1 的极端配置), 其快照返回 nil → isAlreadyDeployed 判否,
	// 走重新部署 (保守方向), 非 bug.
	deploymentUuid         string
	previousDeploymentUuid string
	isDeploying            atomic.Bool
	// The next generation to deploy. nil when there is no new generation to deploy
	GenerationToDeploy *protobuf.Generation
	// The operation to use for the next deployment
	Operation string
	// Reason is the reason of the next deployment
	Reason                string
	generationAvailableCh chan struct{}
	postDeploymentCommand string

	isSuspended atomic.Bool
	resumeCh    chan struct{}
	// This is true when the runner is actually suspended. This is
	// mainly used for testing purpose.
	runnerIsSuspended atomic.Bool
	store             *store.Store
	broker            *broker.Broker
}

// State 返回 deployer 状态快照. 嵌套对象 (Deployment 等) 经 store 锁内快照
// 读取, 调用方可安全长期持有/序列化 (供 manager.toState 聚合后异步 marshal).
func (d *Deployer) State() *protobuf.Deployer {
	d.mu.Lock()
	deploymentUuid := d.deploymentUuid
	previousUuid := d.previousDeploymentUuid
	generation := proto.CloneOf(d.GenerationToDeploy)
	operation := d.Operation
	d.mu.Unlock()
	return &protobuf.Deployer{
		IsDeploying:        wrapperspb.Bool(d.isDeploying.Load()),
		GenerationToDeploy: generation,
		Operation:          operation,
		Deployment:         d.store.GetDeploymentSnapshot(deploymentUuid),
		PreviousDeployment: d.store.GetDeploymentSnapshot(previousUuid),
		IsSuspended:        wrapperspb.Bool(d.isSuspended.Load()),
	}
}

// Deployment 返回当前 deployment 的 store 锁内快照 (不暴露 store 活指针).
func (d *Deployer) Deployment() *protobuf.Deployment {
	d.mu.Lock()
	uuid := d.deploymentUuid
	d.mu.Unlock()
	return d.store.GetDeploymentSnapshot(uuid)
}

func (d *Deployer) IsDeploying() bool {
	return d.isDeploying.Load()
}

func (d *Deployer) RunnerIsSuspended() bool {
	return d.runnerIsSuspended.Load()
}

func (d *Deployer) IsSuspended() bool {
	return d.isSuspended.Load()
}

func showDeployment(padding string, d *protobuf.Deployment) {
	switch d.Status {
	case store.StatusToString(store.Running):
		fmt.Printf("%sDeployment is running since %s\n", padding, humanize.Time(d.StartedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
	case store.StatusToString(store.Done):
		fmt.Printf("%sDeployment succeeded %s\n", padding, humanize.Time(d.EndedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
		fmt.Printf("%sProfilePath %s\n", padding, d.ProfilePath)
	case store.StatusToString(store.Failed):
		fmt.Printf("%sDeployment failed %s\n", padding, humanize.Time(d.EndedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
		fmt.Printf("%sProfilePath %s\n", padding, d.ProfilePath)
	}
	fmt.Printf("%sGeneration %s\n", padding, d.Generation.Uuid)
	if d.Generation.Source != nil && d.Generation.Source.GetGit() != nil {
		git := d.Generation.Source.GetGit()
		fmt.Printf("%sCommit ID %s from %s/%s\n", padding, git.SelectedCommitId, git.SelectedRemoteName, git.SelectedBranchName)
		fmt.Printf("%sCommit message %s\n", padding, strings.Trim(git.SelectedCommitMsg, "\n"))
	}
	fmt.Printf("%sOutpath %s\n", padding, d.Generation.OutPath)
}

func Show(s *protobuf.Deployer, padding string) {
	fmt.Printf("  Deployer\n")
	if s.Deployment == nil {
		if s.PreviousDeployment == nil {
			fmt.Printf("%sNo deployment yet\n", padding)
			return
		}
		showDeployment(padding, s.PreviousDeployment)
		return
	}
	showDeployment(padding, s.Deployment)
}

func New(store *store.Store, deployFunc DeployFunc, previousDeployment *protobuf.Deployment, postDeploymentCommand string, b *broker.Broker) *Deployer {
	if previousDeployment != nil {
		logrus.Infof("deployer: initializing with previous deployment %s", previousDeployment.Uuid)
	}
	deployer := &Deployer{
		store:                 store,
		DeploymentDoneCh:      make(chan *protobuf.Deployment, 1),
		deployerFunc:          deployFunc,
		generationAvailableCh: make(chan struct{}, 1),
		postDeploymentCommand: postDeploymentCommand,
		broker:                b,

		resumeCh: make(chan struct{}, 1),
	}
	// 初始 deployment 指针仅用于取 uuid (启动恢复路径, 无并发).
	if previousDeployment != nil {
		deployer.previousDeploymentUuid = previousDeployment.Uuid
		deployer.deploymentUuid = previousDeployment.Uuid
	}

	dState := store.GetState().Deployer
	isSuspended := dState.IsSuspended
	if isSuspended {
		logrus.Infof("deployer: suspended because of %s", dState.SuspendReason)
		deployer.isSuspended.Store(true)
	}

	return deployer
}

func (d *Deployer) Suspend(reason string) {
	d.isSuspended.Store(true)
	d.store.DeployerUpdate(&protobuf.DeployerState{
		IsSuspended:   true,
		SuspendReason: reason,
	})
}

func (d *Deployer) Resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.isSuspended.Store(false)
	d.store.DeployerUpdate(&protobuf.DeployerState{
		IsSuspended:   false,
		SuspendReason: "",
	})
	select {
	case d.resumeCh <- struct{}{}:
	default:
	}
}

// IsAlreadyDeployed 判断 generation 是否与上一次成功部署等价 (外部入口, 自行加锁).
func (d *Deployer) IsAlreadyDeployed(generation *protobuf.Generation, operation string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isAlreadyDeployedLocked(generation, operation)
}

// isAlreadyDeployedLocked 需调用方持有 d.mu (Submit 在临界区内调用, 不可重入加锁).
func (d *Deployer) isAlreadyDeployedLocked(generation *protobuf.Generation, operation string) bool {
	previous := d.store.GetDeploymentSnapshot(d.previousDeploymentUuid)
	if previous == nil {
		logrus.Infof("deployer: no previous deployment found for generation %s", generation.Uuid)
		return false
	}
	if generation.OutPath != previous.Generation.OutPath {
		logrus.Infof("deployer: out path %s differs from previous out path %s for generation %s", generation.OutPath, previous.Generation.OutPath, generation.Uuid)
		return false
	}
	if previous.Operation != operation {
		logrus.Infof("deployer: operation %s differs from previous operation %s for generation %s", operation, previous.Operation, generation.Uuid)
		return false
	}
	logrus.Infof("deployer: skipping deployment of generation %s: out path %s with operation %s has already been deployed", generation.Uuid, generation.OutPath, operation)
	return true
}

// Submit submits a generation to be deployed. If a deployment is
// running, this generation will be deployed once the current
// deployment is finished. If this generation is the same than the one
// of the last deployment, this generation is skipped.
func (d *Deployer) Submit(generation *protobuf.Generation, operation string, force bool, reason string) {
	forceStr := "false"
	if force {
		forceStr = "true"
	}
	logrus.Infof("deployer: submitting generation %s with operation %s (force: %s)", generation.Uuid, operation, forceStr)
	d.mu.Lock()
	defer d.mu.Unlock()

	if force || !d.isAlreadyDeployedLocked(generation, operation) {
		d.GenerationToDeploy = generation
		d.Operation = operation
		d.Reason = reason
		select {
		case d.generationAvailableCh <- struct{}{}:
		default:
		}
	}
}

func (d *Deployer) Run(ctx context.Context) {
	go func() {
		for {
			var cominNeedRestart bool
			var profilePath string
			var err error
			<-d.generationAvailableCh

			if d.isSuspended.Load() {
				d.runnerIsSuspended.Store(true)
				<-d.resumeCh
				d.runnerIsSuspended.Store(false)
			}

			d.mu.Lock()
			g := d.GenerationToDeploy
			operationSubmitted := d.Operation
			reason := d.Reason
			d.GenerationToDeploy = nil
			d.mu.Unlock()
			logrus.Infof("deployer: deploying generation %s with the submitted operation %s", g.Uuid, operationSubmitted)
			booted, current := utils.GetBootedAndCurrentStorepaths()
			dpl := d.store.NewDeployment(g, operationSubmitted, reason, booted, current)
			operationComputed := dpl.Operation
			d.mu.Lock()
			// 旧的 current 转为 previous; 只记录 uuid, 对象读取走 store 快照.
			d.previousDeploymentUuid = d.deploymentUuid
			d.deploymentUuid = dpl.Uuid
			d.isDeploying.Store(true)
			d.mu.Unlock()
			if err := d.store.DeploymentStarted(dpl.Uuid, booted, current); err != nil {
				logrus.Errorf("deployer: could not update the deployment %s in the store", dpl.Uuid)
				continue
			}
			if operationComputed != types.OperationNull {
				profilePaths := d.store.GetDeploymentProfilePaths()
				stdout, stderr := d.broker.GetLogger("deployment", g.Uuid)
				cominNeedRestart, profilePath, err = d.deployerFunc(
					ctx,
					g.OutPath,
					operationComputed,
					profilePaths,
					stdout,
					stderr,
				)
				// We close the writers to stop associated goroutines
				_ = stdout.Close()
				_ = stderr.Close()
			}
			if err := d.store.DeploymentFinished(dpl.Uuid, err, cominNeedRestart, profilePath, booted, current); err != nil {
				logrus.Errorf("deployer: could not update the deployment %s in the store", dpl.Uuid)
				continue
			}
			// 终态快照 (锁内读取, 含 EndedAt/Status/ErrorMsg/ProfilePath):
			// 供 post deployment command 与 DeploymentDoneCh 下游只读,
			// 不再持有 store 活指针 (EndedAt/Status 由 DeploymentFinished 锁内写入).
			deployment := d.store.GetDeploymentSnapshot(dpl.Uuid)
			cmd := d.postDeploymentCommand
			if cmd != "" {
				// TODO: we should also log these outputs
				_, err = runPostDeploymentCommand(cmd, deployment)
				if err != nil {
					logrus.Errorf("deployer: deploying generation %s, post deployment command [%s] failed %v", g.Uuid, cmd, err)
				}
			}

			d.isDeploying.Store(false)
			d.DeploymentDoneCh <- deployment
		}
	}()
}
