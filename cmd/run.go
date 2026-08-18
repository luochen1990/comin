package cmd

import (
	"os"
	"path"
	"time"

	brokerPkg "github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/config"
	"github.com/nlewo/comin/internal/deployer"
	executorPkg "github.com/nlewo/comin/internal/executor"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/http"
	"github.com/nlewo/comin/internal/manager"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/server"
	storePkg "github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// getGitFromGeneration is a helper function to safely extract Git source from a Generation
func getGitFromGeneration(g *protobuf.Generation) *protobuf.Git {
	if g != nil && g.Source != nil {
		return g.Source.GetGit()
	}
	return &protobuf.Git{}
}

var configFilepath string

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run comin to deploy your published configurations",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.Read(configFilepath)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}

		gitConfig := config.MkGitConfig(cfg)

		if err := os.MkdirAll(cfg.StateDir, os.ModePerm); err != nil {
			logrus.Errorf("Failed to create the state dir %s: %s", cfg.StateDir, err)
			return
		}
		// TODO: this could be removed from release > 0.9.0.
		//
		// Previous comin versions didn't correctly set the
		// permissions of the /var/lib/comin folder. This
		// means we need to explicitly chown them to fix this
		// for existing comin deployments.
		if err := os.Chmod(cfg.StateDir, 0755); err != nil {
			logrus.Errorf("Failed to chmod the state dir %s: %s", cfg.StateDir, err)
			return
		}

		var executor executorPkg.Executor
		executor, err = executorPkg.New(cfg.RepositoryType, gitConfig.Path, gitConfig.Submodules)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}

		machineId, err := executor.ReadMachineId()
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}

		metrics := prometheus.New()
		storeFilename := path.Join(cfg.StateDir, "store.json")
		gcRootsDir := path.Join(cfg.StateDir, "gcroots")
		broker := brokerPkg.New()
		broker.Start()
		store, err := storePkg.New(broker, storeFilename, gcRootsDir, cfg.Retention.DeploymentBootEntryCapacity, cfg.Retention.DeploymentSuccessfulCapacity, cfg.Retention.DeploymentAnyCapacity)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
		if err := store.Load(); err != nil {
			logrus.Errorf("Ignoring the state file %s because of the loading error: %s", storeFilename, err)
		}
		metrics.SetBuildInfo(cmd.Version)

		// We get the last mainCommitId to avoid useless
		// redeployment as well as non fast forward checkouts
		var mainCommitId string
		var lastDeployment *protobuf.Deployment
		if ok, ld := store.LastDeployment(); ok {
			git := getGitFromGeneration(ld.Generation)
			mainCommitId = git.MainCommitId
			lastDeployment = ld
			metrics.SetDeploymentInfo(git.SelectedCommitId, ld.Status)
		}
		repository, err := repository.New(gitConfig, mainCommitId, metrics)
		if err != nil {
			logrus.Errorf("Failed to initialize the repository: %s", err)
			os.Exit(1)
		}

		fetcher := fetcher.NewGitFetcher(repository, broker)
		fetcher.Start(cmd.Context())
		sched := scheduler.New()
		sched.FetchRemotes(fetcher, cfg.Remotes)

		builder := builder.New(store, executor, broker, gitConfig.Path, gitConfig.Dir, cfg.SystemAttr, cfg.Hostname, gitConfig.Submodules, time.Duration(cfg.EvalTimeout)*time.Second, time.Duration(cfg.BuildTimeout)*time.Second)
		deployer := deployer.New(store, executor.Deploy, lastDeployment, cfg.PostDeploymentCommand, broker)

		mode, err := manager.ParseMode(cfg.BuildConfirmer.Mode)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
		buildConfirmer := manager.NewConfirmer(broker, mode, time.Duration(cfg.BuildConfirmer.AutoDuration)*time.Second, "build")
		buildConfirmer.Start()
		mode, err = manager.ParseMode(cfg.DeployConfirmer.Mode)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
		deployConfirmer := manager.NewConfirmer(broker, mode, time.Duration(cfg.DeployConfirmer.AutoDuration)*time.Second, "deploy")
		// 保守策略: needs-reboot generation 的 deploy confirmation 降级 (详见 internal/manager/reboot_policy.go).
		// 判定阈值复用 rebootConfirmer.triggers (SSOT); 咨询回调走 store+executor,
		// 与 manager 在 BuildFinished 时算 pendingChecks 是同一份事实.
		// consult 失败 (查不到 generation/OutPath 空) 选 fail-open (返回 false, 照常 auto-confirm):
		// submit 路径来自刚成功 GenerationGet 的 BuildDone, 失败概率极低; 而 fail-closed 会把
		// 每个 lookup 异常都变成无限等待, 可用性损失大于误放行的风险.
		if err := deployConfirmer.SetRebootPolicy(cfg.DeployConfirmer.RebootPolicy, func(generationUuid string) bool {
			g, err := store.GenerationGet(generationUuid)
			if err != nil || g.OutPath == "" {
				logrus.Warnf("run: cannot consult reboot checks for generation %s: %v", generationUuid, err)
				return false
			}
			return manager.NeedsRebootConfirm(executor.CheckReboot(g.OutPath), cfg.RebootConfirmer.Triggers)
		}); err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
		deployConfirmer.Start()

		configurationOperations := manager.ConfigurationOperations{}
		for _, r := range cfg.Remotes {
			configurationOperations[r.Name] = make(map[string]string)
			configurationOperations[r.Name][r.Branches.Main.Name] = r.Branches.Main.Operation
			configurationOperations[r.Name][r.Branches.Testing.Name] = r.Branches.Testing.Operation
		}
		manager := manager.New(store, metrics, sched, fetcher, builder, deployer, machineId, cfg.Hostname, executor, buildConfirmer, deployConfirmer, broker, configurationOperations)

		http.Serve(manager,
			metrics,
			cfg.ApiServer.ListenAddress, cfg.ApiServer.Port,
			cfg.Exporter.ListenAddress, cfg.Exporter.Port)
		srv := server.New(broker, manager, cfg.Grpc.UnixSocketPath)
		srv.Start()

		prometheus.Subscribe(broker, &metrics)

		manager.Run(cmd.Context())
	},
}

func init() {
	runCmd.PersistentFlags().StringVarP(&configFilepath, "config", "", "", "the configuration file path")
	_ = runCmd.MarkPersistentFlagRequired("config")
	rootCmd.AddCommand(runCmd)
}
