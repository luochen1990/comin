package types

const OperationTest = "test"
const OperationSwitch = "switch"
const OperationBoot = "boot"
const OperationNull = "null"

type Remote struct {
	Name     string
	URL      string
	Auth     Auth
	Branches Branches `yaml:"branches"`
	Timeout  int      `yaml:"timeout"`
	// The period to poll the remote in second
	Poller Poller `yaml:"poller"`
}

type Poller struct {
	Period int `yaml:"period"`
}

type GitConfig struct {
	// The repository Path
	Path string
	// The directory in the repository
	Dir                   string
	Remotes               []Remote
	GpgPublicKeyPaths     []string
	SshAllowedSignersPath string
	Submodules            bool
}

type Auth struct {
	AccessToken       string
	AccessTokenPath   string `yaml:"access_token_path"`
	SshDeployKeyPath  string `yaml:"ssh_deploy_key_path"`
	SshKnownHostsPath string `yaml:"ssh_known_hosts_path"`
	Username          string
}

type Branch struct {
	Name string `yaml:"name"`
	// TODO: use it
	Protected bool   `yaml:"protected"`
	Operation string `yaml:"operation"`
}

type Branches struct {
	Main    Branch `yaml:"main"`
	Testing Branch `yaml:"testing"`
}

type HttpServer struct {
	ListenAddress string `yaml:"listen_address"`
	Port          int    `yaml:"port"`
}

type Grpc struct {
	UnixSocketPath string `yaml:"unix_socket_path"`
}

type Confirmer struct {
	Mode         string `yaml:"mode"`
	AutoDuration int    `yaml:"autoconfirm_duration"`
	// RebootPolicy 是 needs-reboot generation 的部署保守策略 (仅 deploy confirmer 消费):
	//   ""       不干预 (默认, 超时照常放行)
	//   "manual" 降级为无限等待用户确认
	//   "skip"   倒计时归零转入 manual 等待 (不取消), 仅用户显式确认才部署
	// 判定阈值复用 RebootConfirmer.Triggers (SSOT, 见 internal/manager/reboot_policy.go).
	RebootPolicy string `yaml:"reboot_policy"`
}

// RebootConfirmer 控制 needs-reboot 交互通知的行为.
// 与 build/deploy confirmer 不同: reboot 不阻塞主流程 (deploy 完成即视为成功),
// 仅是给用户一个交互窗口决定是否立即重启.
//
// 注: 此类型当前由 comin-config.nix 序列化为 YAML, 但 Go 主进程并未消费 (reboot 配置
// 通过 systemd 环境变量直接注入 desktop service). 保留此类型作为 YAML schema 的 Go 端
// 镜像, 避免 config 解析在其他字段消费时因 unknown field 报错.
type RebootConfirmer struct {
	Mode                string `yaml:"mode"`
	AutoconfirmDuration int    `yaml:"autoconfirm_duration"`
	AutoconfirmAction   string `yaml:"autoconfirm_action"`
	// Triggers 列出哪些 RebootChecks 字段为 true 时才弹交互式通知.
	// 取值为字段名的 kebab-case: kernel-changed / initrd-changed / kernel-modules-changed /
	// systemd-abi-changed / systemd-upgraded.
	// 默认空列表 = 任意字段为 true 都触发 (即 Any()).
	Triggers []string `yaml:"triggers"`
}

type Retention struct {
	DeploymentBootEntryCapacity  int `yaml:"deployment_boot_entry_capacity"`
	DeploymentSuccessfulCapacity int `yaml:"deployment_successful_capacity"`
	DeploymentAnyCapacity        int `yaml:"deployment_any_capacity"`
}

type Configuration struct {
	Hostname      string `yaml:"hostname"`
	StateDir      string `yaml:"state_dir"`
	StateFilepath string `yaml:"state_filepath"`
	// RepositoryType describes type of the repository. It can currently only be "flake"
	RepositoryType        string          `yaml:"repository_type"`
	RepositorySubdir      string          `yaml:"repository_subdir"`
	Submodules            bool            `yaml:"submodules"`
	SystemAttr            string          `yaml:"system_attr"`
	Remotes               []Remote        `yaml:"remotes"`
	ApiServer             HttpServer      `yaml:"api_server"`
	Grpc                  Grpc            `yaml:"grpc"`
	Exporter              HttpServer      `yaml:"exporter"`
	GpgPublicKeyPaths     []string        `yaml:"gpg_public_key_paths"`
	SshAllowedSignersPath string          `yaml:"ssh_allowed_signers_path"`
	PostDeploymentCommand string          `yaml:"post_deployment_command"`
	BuildConfirmer        Confirmer       `yaml:"build_confirmer"`
	DeployConfirmer       Confirmer       `yaml:"deploy_confirmer"`
	RebootConfirmer       RebootConfirmer `yaml:"reboot_confirmer"`
	Retention             Retention       `yaml:"retention"`
	EvalTimeout           int             `yaml:"eval_timeout"`
	BuildTimeout          int             `yaml:"build_timeout"`
	// FingerprintCachePath 指向 scripts/build 维护的指纹缓存 (空 = 禁用).
	// 命中时 Eval 跳过 nix 求值, 见 internal/executor/fingerprint.go.
	FingerprintCachePath string `yaml:"fingerprint_cache_path"`
}
