package executor

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
)

type EvalFunc func(ctx context.Context, source *protobuf.Source, stdout, stderr io.WriteCloser) (drvPath string, outPath string, machineId string, err error)
type BuildFunc func(ctx context.Context, drvPath string, stdout, stdin io.WriteCloser) error

// fingerprintCachePath 参数指向 scripts/build 的指纹缓存文件; 空串禁用
// 快路径 ( Eval 不再尝试跳过求值). 见 internal/executor/fingerprint.go.
func New(repositoryType, repositoryPath string, submodules bool, fingerprintCachePath string) (e Executor, err error) {
	switch repositoryType {
	case "flake":
		if runtime.GOOS == "darwin" {
			return NewGitNixFlakeDarwin(repositoryPath, submodules, fingerprintCachePath)
		} else {
			return NewGitNixFlakeNixOS(repositoryPath, submodules, fingerprintCachePath)
		}

	case "nix":
		return NewGitNixNixOS(repositoryPath, submodules)
	}
	return e, fmt.Errorf("failed to create the executor: %s", err)
}

// Executor contains the function used by comin to actually do actions
// on the host. This allows us to abstract the way Nix expression are
// evaluated, built and deployed. This could be for instance used by a
// Garnix implementation (such as proposed in
// https://github.com/nlewo/comin/pull/74)
type Executor interface {
	Eval(ctx context.Context, source *protobuf.Source, stdout, stderr io.WriteCloser) (drvPath string, outPath string, machineId string, err error)
	Build(ctx context.Context, drvPath string, stdout, stdin io.WriteCloser) (err error)
	Deploy(ctx context.Context, outPath, operation string, profilePaths []string, stdout, stderr io.WriteCloser) (needToRestartComin bool, profilePath string, err error)
	// CheckReboot 比较 booted-system 与 outPath, 返回结构化的 reboot 检查事实.
	// 纯事实陈述, 不做聚合; manager 层负责单调累积为 RebootStatus.
	// outPath 通常是刚 build 出来的 system generation store path (BuildFinished 即可调用).
	CheckReboot(outPath string) *protobuf.RebootChecks
	ReadMachineId() (string, error)
	// IsStorePathExist returns true if a storepath exists. This
	// is used to detect if a build will be required or not.
	IsStorePathExist(string) bool
}

// fingerprintCachePath 参数指向 scripts/build 的指纹缓存文件; 空串禁用
// 快路径 ( Eval 不再尝试跳过求值). 见 internal/executor/fingerprint.go.
func NewGitNixFlakeNixOS(repositoryPath string, submodules bool, fingerprintCachePath string) (e Executor, err error) {
	logrus.Info("executor: creating a NixOS flake executor")
	e, err = NewGitNixFlake("nixosConfigurations", repositoryPath, submodules, fingerprintCachePath)
	return
}
func NewGitNixFlakeDarwin(repositoryPath string, submodules bool, fingerprintCachePath string) (e Executor, err error) {
	logrus.Info("executor: creating a nix-darwin flake executor")
	e, err = NewGitNixFlake("darwinConfigurations", repositoryPath, submodules, fingerprintCachePath)
	return
}

func NewGitNixNixOS(repositoryPath string, submodules bool) (e Executor, err error) {
	logrus.Info("executor: creating a NixOS executor")
	e, err = NewGitNix(repositoryPath, submodules)
	return
}
