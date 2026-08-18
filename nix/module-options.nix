{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.services.comin;
in
{
  imports = [
    (lib.mkRenamedOptionModule
      [ "services" "comin" "flakeSubdirectory" ]
      [ "services" "comin" "repositorySubdir" ]
    )
    (lib.mkIf cfg.enable {
      assertions = [
        {
          assertion = cfg.hostname != null && cfg.hostname != "";
          message = "You must set `networking.hostName` or `services.comin.hostname` explicitly in your NixOS configuration.";
        }
        {
          assertion = cfg.repositoryType == "nix" || cfg.repositoryType == "flake" && cfg.systemAttr == null;
          message = "When the `services.comin.repositoryType` is `flake`, the configuration attribute `services.comin.systemAttr` must not be set.";
        }
        {
          assertion = cfg.repositoryType == "flake" || cfg.repositoryType == "nix" && cfg.systemAttr != null;
          message = "When the `services.comin.repositoryType` is `nix`, the the configuration attribute `services.comin.systemAttr` must be set.";
        }
      ];
    })
  ];
  options =
    with lib;
    with types;
    {
      services.comin = {
        enable = mkOption {
          type = types.bool;
          default = false;
          description = ''
            Whether to run the comin service.
          '';
        };
        package = lib.mkPackageOption pkgs "comin" { nullable = true; } // {
          defaultText = "pkgs.comin or comin.packages.\${system}.default or null";
        };
        hostname = mkOption {
          type = str;
          default = config.networking.hostName;
          defaultText = lib.literalExpression "config.networking.hostName";
          description = ''
            The name of the configuration to evaluate and deploy.
            This value is used by comin to evaluate the flake output
            nixosConfigurations."<hostname>" or darwinConfigurations."<hostname>".
            Defaults to networking.hostName - you MUST set either this option
            or networking.hostName in your configuration.
          '';
        };
        repositoryType = mkOption {
          type = enum [
            "flake"
            "nix"
          ];
          default = "flake";
          description = ''
            The type of the repository to fetch. It can either contains a flake or a classical Nix expression.
          '';
        };
        repositorySubdir = mkOption {
          type = str;
          default = ".";
          description = ''
            Subdirectory in the repository, containing a default.nix or a flake.nix file.
          '';
        };
        systemAttr = mkOption {
          type = nullOr str;
          default = null;
          description = ''
            This is the attribute containing the machine toplevel
            attribute. Note this is only used when the repositoryType is
            'nix'. When the repository type is 'flake', the attribute is
            derived from the hostname.
          '';
        };
        submodules = mkOption {
          type = bool;
          default = false;
          description = ''
            Whether to fetch and include Git submodules when cloning the repository.
            When enabled, this adds ?submodules=1 to the flake URL.
          '';
        };
        evalTimeout = mkOption {
          type = int;
          default = 1800;
          description = ''
            Maximum duration in seconds for a Nix evaluation (flake/nix eval)
            before comin cancels it.
          '';
        };
        buildTimeout = mkOption {
          type = int;
          default = 1800;
          description = ''
            Maximum duration in seconds for a Nix build before comin cancels it.
          '';
        };
        exporter = mkOption {
          description = "Options for the Prometheus exporter.";
          default = { };
          type = submodule {
            options = {
              listen_address = mkOption {
                type = str;
                description = ''
                  Address to listen on for the Prometheus exporter. Empty string will listen on all interfaces.
                '';
                default = "";
              };
              port = mkOption {
                type = int;
                description = ''
                  Port to listen on for the Prometheus exporter.
                '';
                default = 4243;
              };
              openFirewall = mkOption {
                type = types.bool;
                default = false;
                description = ''
                  Open port in firewall for incoming connections to the Prometheus exporter.
                '';
              };
            };
          };
        };
        remotes = mkOption {
          description = "Ordered list of repositories to pull.";
          type = listOf (submodule {
            options = {
              name = mkOption {
                type = str;
                description = ''
                  The name of the remote.
                '';
              };
              url = mkOption {
                type = str;
                description = ''
                  The URL of the repository.
                '';
              };
              auth = mkOption {
                description = "Authentication options.";
                default = { };
                type = submodule {
                  options = {
                    access_token_path = mkOption {
                      type = str;
                      default = "";
                      description = ''
                        The path of the auth file.
                      '';
                    };
                    username = mkOption {
                      type = str;
                      default = "comin";
                      description = ''
                        The username used to authenticate to the Git
                        remote repository. Note that any non empty
                        username is valid on GitLab and GitHub.
                      '';
                    };
                    ssh_deploy_key_path = mkOption {
                      type = str;
                      default = "";
                      description = ''
                        Path to the SSH private key used to authenticate to the Git remote.
                      '';
                    };
                    ssh_known_hosts_path = mkOption {
                      type = str;
                      default = "";
                      description = ''
                        Path to the known_hosts file used to verify the SSH
                        host key of the Git remote. Defaults to
                        /etc/ssh/ssh_known_hosts when unset. The remote's host
                        key must be present in this file.
                      '';
                    };
                  };
                };
              };
              timeout = mkOption {
                type = int;
                default = 300;
                description = ''
                  Git fetch timeout in seconds.
                '';
              };
              branches = mkOption {
                description = "Branches to pull.";
                default = { };
                type = submodule {
                  options = {
                    main = mkOption {
                      default = { };
                      description = "The main branch to fetch.";
                      type = submodule {
                        options = {
                          name = mkOption {
                            type = str;
                            default = "main";
                            description = "The name of the main branch.";
                          };
                          operation = mkOption {
                            type = enum [
                              "switch"
                              "test"
                              "boot"
                            ];
                            default = "switch";
                            description = "The switch-to-configuration operation to do on this branch.";
                          };
                        };
                      };
                    };
                    testing = mkOption {
                      default = { };
                      description = "The testing branch to fetch.";
                      type = submodule {
                        options = {
                          name = mkOption {
                            type = str;
                            default = "testing-${config.services.comin.hostname}";
                            defaultText = lib.literalExpression "testing-\${config.services.comin.hostname}";
                            description = "The name of the testing branch.";
                          };
                          operation = mkOption {
                            type = enum [
                              "switch"
                              "test"
                              "boot"
                            ];
                            default = "test";
                            description = "The switch-to-configuration operation to do on this branch.";
                          };
                        };
                      };
                    };
                  };
                };
              };
              poller = mkOption {
                default = { };
                description = "The poller options.";
                type = submodule {
                  options = {
                    period = mkOption {
                      type = types.int;
                      default = 60;
                      description = ''
                        The poller period in seconds.
                      '';
                    };
                  };
                };
              };
            };
          });
        };
        debug = mkOption {
          type = types.bool;
          default = false;
          description = ''
            Whether to run comin in debug mode. Be careful, secrets are shown!.
          '';
        };
        machineId = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            The expected machine-id of the machine configured by
            comin. If not null, the configuration is only deployed
            when this specified machine-id is equal to the actual
            machine-id.
            This is mainly useful for server migration: this allows
            to migrate a configuration from a machine to another
            machine (with different hardware for instance) without
            impacting both.
            Note it is only used by comin at evaluation.
          '';
        };
        gpgPublicKeyPaths = mkOption {
          description = "A list of GPG public key file paths. Each of this file should contains an armored GPG key.";
          type = listOf str;
          default = [ ];
        };
        sshAllowedSignersPath = mkOption {
          description = "An OpenSSH allowed signers file path used to verify SSH-signed Git commits.";
          type = nullOr str;
          default = null;
        };
        postDeploymentCommand = mkOption {
          description = "A path to a script executed after each
        deployment. comin provides to the script the following
        environment variables: `COMIN_GIT_SHA`, `COMIN_GIT_REF`,
        `COMIN_GIT_MSG`, `COMIN_HOSTNAME`, `COMIN_FLAKE_URL`,
        `COMIN_GENERATION`, `COMIN_STATUS` and `COMIN_ERROR_MSG`.";
          type = nullOr path;
          default = null;
          example = lib.literalExpression ''
            pkgs.writers.writeBash "post" "echo $COMIN_GIT_SHA";
          '';
        };
        buildConfirmer = mkOption {
          description = "The confirmer options for the build.";
          default = { };
          type = submodule {
            options = {
              mode = mkOption {
                type = enum [
                  "without"
                  "auto"
                  "manual"
                ];
                default = "without";
                description = ''
                  The confirmer mode. "without" immediately confirms
                  without any user interaction. "manual" requires a user
                  confirmation. "auto" automatically confirms after
                  waiting for the autoconfirm_duration.
                '';
              };
              autoconfirm_duration = mkOption {
                type = int;
                default = 120;
                description = ''
                  The autoconfirm timer duration in seconds. After this
                  duration, the action is automatically confirmed.
                '';
              };

            };
          };
        };
        deployConfirmer = mkOption {
          description = "The confirmer options for the deployment.";
          default = { };
          type = submodule {
            options = {
              mode = mkOption {
                type = enum [
                  "without"
                  "auto"
                  "manual"
                ];
                default = "without";
                description = ''
                  The confirmer mode. "without" immediately confirms
                  without any user interaction. "manual" requires a user
                  confirmation. "auto" automatically confirms after
                  waiting for the autoconfirm_duration.
                '';
              };
              autoconfirm_duration = mkOption {
                type = int;
                default = 120;
                description = ''
                  The autoconfirm timer duration in seconds. After this
                  duration, the action is automatically confirmed.
                '';
              };
              # reboot_policy: needs-reboot generation 的保守部署策略.
              # 与 rebootConfirmer 共用判定阈值 (triggers, SSOT): desktop 通知里
              # 出现 "⚠ 切换后需要重启" 提示的 generation, 就会被此策略拦截.
              reboot_policy = mkOption {
                type = enum [
                  null
                  "manual"
                  "skip"
                ];
                default = null;
                description = ''
                  Conservative policy for generations that need a reboot
                  (per rebootConfirmer.triggers) to take effect. Only
                  relevant when mode = "auto".
                  null (default): no special handling - the autoconfirm
                  timer confirms the deployment as usual.
                  "skip": on timer expiry, fall back to waiting for the
                  user instead of confirming. The generation is only
                  deployed when the user explicitly confirms (e.g. the
                  "Deploy now" desktop notification button); the
                  notification stays with its action buttons.
                  "manual": wait indefinitely for user confirmation
                  (downgrade auto to manual for these generations).
                '';
              };

            };
          };
        };
        # rebootConfirmer: needs-reboot 交互通知配置.
        # 与 build/deploy confirmer 不同: reboot 不阻塞主流程 (deploy 完成 = 部署成功),
        # 此 option 仅控制部署完成后若检测到 needs-reboot, 是否弹出交互通知及超时行为.
        rebootConfirmer = mkOption {
          description = "The confirmer options for the reboot prompt after deployment.";
          default = { };
          type = submodule {
            options = {
              mode = mkOption {
                type = enum [
                  "without"
                  "auto"
                  "manual"
                ];
                default = "auto";
                description = ''
                  The reboot notification mode.
                  "without": no interactive prompt, only a transient notification (legacy behavior).
                  "manual": persistent interactive notification, waits indefinitely for user action.
                  "auto": persistent interactive notification with countdown; on timeout performs autoconfirm_action.
                '';
              };
              autoconfirm_duration = mkOption {
                type = int;
                default = 300;
                description = ''
                  The autoconfirm timer duration in seconds for the reboot prompt.
                  Only effective when mode = "auto". Default 300s (5 min) - longer than
                  deploy's 120s because reboot is more disruptive.
                '';
              };
              autoconfirm_action = mkOption {
                type = enum [
                  "reboot"
                  "skip"
                ];
                default = "skip";
                description = ''
                  The action to take when the reboot prompt autoconfirm timer expires.
                  Only effective when mode = "auto".
                  "skip" (default, conservative): dismiss the notification, do not reboot.
                  "reboot": automatically trigger systemctl reboot.
                '';
              };
              triggers = mkOption {
                type = listOf str;
                default = [
                  "kernel-changed"
                  "initrd-changed"
                  "kernel-modules-changed"
                  "systemd-abi-changed"
                ];
                description = ''
                  Which RebootChecks fields trigger the interactive reboot notification.
                  Values are field names in kebab-case. Available fields:
                  - kernel-changed, initrd-changed, kernel-modules-changed, systemd-abi-changed:
                    hard reboot requirements (content changed, reboot needed to take effect).
                  - systemd-upgraded:
                    soft reboot suggestion (new systemd version, PID 1 still runs old version).
                  Default excludes systemd-upgraded (only hard requirements trigger prompt).
                  Add "systemd-upgraded" to also be prompted on systemd version bumps.
                  Empty list [] disables the reboot notification entirely.
                '';
              };
            };
          };
        };
        desktop = {
          enable = mkEnableOption "Whether to run the comin desktop service. This user service send notifications over DBus.";
          title = mkOption {
            type = str;
            default = "comin";
            description = "The notification title.";
          };
        };
        retention = mkOption {
          description = "The deployments and profiles retention policyes.";
          default = { };
          type = submodule {
            options = {
              deployment_boot_entry_capacity = mkOption {
                type = int;
                default = 3;
                description = ''
                  Number of boot entries to keep. Controls how many successful
                  deployments generating boot entries (boot or switch operations)
                  with unique storepaths are retained.
                '';
              };
              deployment_successful_capacity = mkOption {
                type = int;
                default = 3;
                description = ''
                  Number of successful deployments to keep. Includes all deployments
                  with status=done, regardless of operation type.
                '';
              };
              deployment_any_capacity = mkOption {
                type = int;
                default = 5;
                description = ''
                  Total number of deployments to keep. Includes all deployments
                  regardless of status (including failed deployments).
                '';
              };
            };
          };
        };
      };
    };
}
