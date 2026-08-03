{ self }:
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config;
  cominConfigLib = import ./comin-config.nix { inherit config pkgs lib; };
  inherit (cominConfigLib) cominConfigYaml;

  inherit (pkgs.stdenv.hostPlatform) system;
  inherit (cfg.services.comin) package;

  remoteWithAuth = lib.findFirst (r: r.auth.access_token_path != "") null cfg.services.comin.remotes;

  # This is needed because Nix's flake fetcher shells out to git for
  # submodule operations, and git has no other way to authenticate.
  gitAskpass = pkgs.writeShellScript "comin-git-askpass" ''
    case "$1" in
      Username*) echo "${remoteWithAuth.auth.username}" ;;
      Password*) cat "${remoteWithAuth.auth.access_token_path}" ;;
    esac
  '';
in
{
  imports = [ ./module-options.nix ];
  config = lib.mkIf cfg.services.comin.enable {
    assertions = [
      {
        assertion = package != null;
        message = "`services.comin.package` cannot be null.";
      }
      # If the package is null and our `system` isn't supported by the Flake, it's probably safe to show this error message
      {
        assertion = package == null -> lib.elem system (lib.attrNames self.packages);
        message = "comin: ${system} is not supported by the Flake.";
      }
    ]
    ++ lib.forEach cfg.services.comin.remotes (remote: {
      assertion = !(remote.auth.access_token_path != "" && remote.auth.ssh_deploy_key_path != "");
      message = "comin: remote `${remote.name}` sets both `auth.access_token_path` and `auth.ssh_deploy_key_path`; set at most one.";
    });

    systemd.user.services.comin-desktop = lib.mkIf cfg.services.comin.desktop.enable {
      wantedBy = [ "graphical-session.target" ];
      path = [ pkgs.libnotify ];
      serviceConfig = {
        ExecStart = ''${lib.getExe package} desktop --title "${cfg.services.comin.desktop.title}"'';
      };
      # 通过环境变量传递 rebootConfirmer 配置到 desktop 服务.
      # 设计理由: 这些是静态配置 (mode/duration/action/triggers), 不属于运行时状态,
      # 不需要走 gRPC 同步; 直接由 systemd 注入, desktop 启动时一次性读取即可.
      environment =
        let
          rc = cfg.services.comin.rebootConfirmer;
        in
        {
          COMIN_REBOOT_MODE = rc.mode;
          COMIN_REBOOT_AUTOCONFIRM_DURATION = toString rc.autoconfirm_duration;
          COMIN_REBOOT_AUTOCONFIRM_ACTION = rc.autoconfirm_action;
          # triggers 空列表是合法配置 (用户显式禁用 reboot 通知), 必须与"未设置"(走默认值)区分.
          # 用特殊哨兵 "none" 标记空列表, desktop 侧据此真正禁用 (而非回退到默认值).
          COMIN_REBOOT_TRIGGERS = if rc.triggers == [ ] then "none" else lib.concatStringsSep "," rc.triggers;
        };
    };

    environment.systemPackages = [ package ];
    networking.firewall.allowedTCPPorts = lib.optional cfg.services.comin.exporter.openFirewall cfg.services.comin.exporter.port;
    # Use package from overlay first, then Flake package if available
    services.comin.package = lib.mkDefault pkgs.comin or self.packages.${system}.comin or null;
    systemd.services.comin = {
      wantedBy = [ "multi-user.target" ];
      path = [
        config.nix.package
        config.programs.ssh.package
      ];
      # The comin service is restarted by comin itself when it
      # detects the unit file changed.
      restartIfChanged = false;
      environment = {
        CURL_CA_BUNDLE = cfg.security.pki.caBundle;
      }
      // cfg.networking.proxy.envVars
      // (lib.optionalAttrs (cfg.services.comin.submodules && remoteWithAuth != null) {
        GIT_ASKPASS = gitAskpass;
      });
      serviceConfig = {
        ExecStart =
          (lib.getExe package)
          + (lib.optionalString cfg.services.comin.debug " --debug ")
          + " run "
          + "--config ${cominConfigYaml}";
        Restart = "always";
      };
    };
  };
}
