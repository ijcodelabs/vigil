# NixOS module for the vigil agent. NixOS manages /etc/systemd itself, so the
# unit is declared here instead of being copied there.
#
#   imports = [ ./vigil-agent.nix ];
#   services.vigil-agent.enable = true;
#
# The binary and the agent's config are not built by Nix: copy the static
# binary to /opt/vigil/vigil-agent and let `vigil-agent enroll` write
# /etc/vigil/agent.yaml
# (docs/install-by-ai.md), then `nixos-rebuild switch`.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.vigil-agent;
in
{
  options.services.vigil-agent = {
    enable = lib.mkEnableOption "the vigil monitoring agent";
    binary = lib.mkOption {
      type = lib.types.str;
      default = "/opt/vigil/vigil-agent";
      description = "Path of the static vigil-agent binary on the host.";
    };
    configFile = lib.mkOption {
      type = lib.types.str;
      default = "/etc/vigil/agent.yaml";
      description = "The agent's config file, as written by `vigil-agent enroll`.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.vigil-agent = {
      description = "vigil monitoring agent";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      # The agent runs journalctl and systemctl; a NixOS unit has neither on
      # its PATH unless it is put there.
      path = [ pkgs.systemd ];
      # A changed binary does not change this unit; restart by hand after an upgrade.
      serviceConfig = {
        ExecStart = "${cfg.binary} -config ${cfg.configFile}";
        EnvironmentFile = "-/etc/vigil/agent.env";
        StateDirectory = "vigil";
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
