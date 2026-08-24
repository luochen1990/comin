package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/nlewo/comin/internal/utils"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
)

type GitNixFlake struct {
	systemAttr string
	// fingerprintCachePath 指向 scripts/build 的指纹缓存 (空 = 禁用), 见 fingerprint.go.
	fingerprintCachePath string
	repositoryPath       string
	submodules           bool
}

func NewGitNixFlake(systemAttr, repositoryPath string, submodules bool, fingerprintCachePath string) (*GitNixFlake, error) {
	return &GitNixFlake{
		systemAttr:           systemAttr,
		fingerprintCachePath: fingerprintCachePath,
		repositoryPath:       repositoryPath,
		submodules:           submodules,
	}, nil
}

func (n *GitNixFlake) ReadMachineId() (string, error) {
	if n.systemAttr == "darwinConfigurations" {
		return utils.ReadMachineIdDarwin()
	}
	return utils.ReadMachineIdLinux()
}

func (n *GitNixFlake) CheckReboot(outPath string) *protobuf.RebootChecks {
	if n.systemAttr == "darwinConfigurations" {
		// TODO: Implement proper reboot detection for Darwin
		// Unlike NixOS which has /run/current-system vs /run/booted-system paths,
		// Darwin/macOS doesn't have equivalent mechanisms for detecting when
		// a reboot is needed after nix-darwin configuration changes.
		// For now, conservatively report no changes.
		return &protobuf.RebootChecks{}
	}
	return utils.CheckRebootLinux(outPath)
}

func (n *GitNixFlake) IsStorePathExist(storePath string) bool {
	return isStorePathExist(storePath)
}

func (n *GitNixFlake) ShowDerivation(ctx context.Context, flakeUrl, hostname string) (drvPath string, outPath string, err error) {
	return showDerivationWithFlake(ctx, flakeUrl, hostname, n.systemAttr, os.Stdout, os.Stderr)
}

func (n *GitNixFlake) Eval(ctx context.Context, source *protobuf.Source, stdout, stderr io.WriteCloser) (drvPath string, outPath string, machineId string, err error) {
	gitSource := source.GetGit()
	if gitSource == nil {
		return "", "", "", fmt.Errorf("expected Git source, got nil")
	}
	// 指纹缓存快路径: 部署 commit 的 tree 已被本机构建过 (outPath 在 store)
	// 时直接复用求值结果, 跳过下面两次 nix 求值. 仅对 nixosConfigurations
	// 生效 — 缓存生产端 (scripts/build) 只求值该 attrset; guard 用字段而非
	// 参数, 与下方正常求值路径同源. machineId 返回 "" (未设置语义, 跳过
	// machine-id 门禁; 等价保护是缓存按 hostname 键控, 见 fingerprint.go
	// 头注释).
	if n.fingerprintCachePath != "" && n.systemAttr == "nixosConfigurations" {
		if treeHash, terr := commitTreeHash(n.repositoryPath, gitSource.SelectedCommitId); terr == nil {
			if d, o, ok := lookupFingerprint(n.fingerprintCachePath, treeHash, gitSource.Hostname); ok {
				logrus.Infof("nix: fingerprint cache hit: tree %s of host %s already built as %s, skipping evaluation", treeHash, gitSource.Hostname, o)
				return d, o, "", nil
			}
		} else {
			logrus.Debugf("nix: fingerprint tree hash lookup failed (falling back to eval): %s", terr)
		}
	}
	flakeUrl := fmt.Sprintf("git+file://%s?dir=%s&rev=%s", n.repositoryPath, gitSource.RepositorySubdir, gitSource.SelectedCommitId)
	if n.submodules {
		flakeUrl += "&submodules=1"
	}
	drvPath, outPath, err = showDerivationWithFlake(ctx, flakeUrl, gitSource.Hostname, n.systemAttr, stdout, stderr)
	if err != nil {
		return
	}
	machineId, err = getExpectedMachineId(ctx, flakeUrl, gitSource.Hostname, n.systemAttr, stdout, stderr)
	return
}

func (n *GitNixFlake) Build(ctx context.Context, drvPath string, stdout, stdin io.WriteCloser) (err error) {
	return buildWithFlake(ctx, drvPath, stdout, stdin)
}

func (n *GitNixFlake) Deploy(ctx context.Context, outPath, operation string, profilePaths []string, stdout, stderr io.WriteCloser) (needToRestartComin bool, profilePath string, err error) {
	return deploy(ctx, outPath, operation, n.systemAttr, profilePaths, stdout, stderr)
}

type Path struct {
	Path string `json:"path"`
}

type Output struct {
	Out Path `json:"out"`
}

type Derivation struct {
	Outputs Output `json:"outputs"`
}

type DerivationOutput struct {
	Version     int                   `json:"version"`
	Derivations map[string]Derivation `json:"derivations"`
}

type Show struct {
	NixosConfigurations  map[string]struct{} `json:"nixosConfigurations"`
	DarwinConfigurations map[string]struct{} `json:"darwinConfigurations"`
}

func (n *GitNixFlake) List(flakeUrl string) (hosts []string, err error) {
	args := []string{
		"flake",
		"show",
		"--json",
		flakeUrl,
	}
	var stdout bytes.Buffer
	err = runNixFlakeCommand(context.Background(), args, &NopWriteCloser{Writer: &stdout}, &NopWriteCloser{Writer: os.Stderr})
	if err != nil {
		return
	}

	var output Show
	err = json.Unmarshal(stdout.Bytes(), &output)
	if err != nil {
		return
	}

	var configurations map[string]struct{}
	if n.systemAttr == "darwinConfigurations" {
		configurations = output.DarwinConfigurations
	} else {
		configurations = output.NixosConfigurations
	}

	hosts = make([]string, 0, len(configurations))
	for key := range configurations {
		hosts = append(hosts, key)
	}
	return
}
