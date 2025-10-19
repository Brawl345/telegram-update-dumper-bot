{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.telegram-update-dumper-bot;
  defaultUser = "telegramupdatedumperbot";
  inherit (lib)
    mkEnableOption
    mkPackageOption
    mkOption
    mkIf
    types
    optionalAttrs
    ;
in
{
  options.services.telegram-update-dumper-bot = {
    enable = mkEnableOption "Update Dumper bot for Telegram";

    package = mkPackageOption pkgs "telegram-update-dumper-bot" { };

    user = mkOption {
      type = types.str;
      default = defaultUser;
      description = "User under which Telegram Update Dumper Bot runs.";
    };

    botTokenFile = mkOption {
      type = types.path;
      description = "File containing Telegram Bot Token";
    };

  };

  config = mkIf cfg.enable {

    systemd.services.telegram-update-dumper-bot = {
      description = "Telegram Update Dumper Bot";
      after = [
        "network-online.target"
      ];
      requires = [
        "network-online.target"
      ];
      wantedBy = [ "multi-user.target" ];

      script = ''
        export TELEGRAM_BOT_TOKEN="$(< $CREDENTIALS_DIRECTORY/TELEGRAM_BOT_TOKEN )"

        exec ${cfg.package}/bin/telegram-update-dumper-bot
      '';

      serviceConfig = {
        LoadCredential = [
          "TELEGRAM_BOT_TOKEN:${cfg.botTokenFile}"
        ];

        Restart = "always";
        User = cfg.user;
        Group = defaultUser;
      };
    };

    users = optionalAttrs (cfg.user == defaultUser) {
      users.${defaultUser} = {
        isSystemUser = true;
        group = defaultUser;
        description = "Telegram Update Dumper Bot user";
      };

      groups.${defaultUser} = { };
    };

  };

}
