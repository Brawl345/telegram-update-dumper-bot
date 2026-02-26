{
  description = "Telegram update dumper bot";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixpkgs-unstable";
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      forAllSystems =
        function:
        nixpkgs.lib.genAttrs [
          "x86_64-linux"
          "aarch64-linux"
          "x86_64-darwin"
          "aarch64-darwin"
        ] (system: function nixpkgs.legacyPackages.${system});

      version = if (self ? shortRev) then self.shortRev else "dev";
    in
    {

      nixosModules = {
        default = ./module.nix;
      };

      overlays.default = final: prev: {
        telegram-update-dumper-bot = self.packages.${prev.stdenv.hostPlatform.system}.default;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
          ];
        };
      });

      packages = forAllSystems (pkgs: {
        telegram-update-dumper-bot = pkgs.buildGoModule {
          pname = "telegram-update-dumper-bot";
          inherit version;
          src = pkgs.lib.cleanSource self;

          vendorHash = null;

          ldflags = [
            "-s"
            "-w"
          ];

          meta = {
            description = "Replies to every message with the update JSON from the Telegram Bot API";
            homepage = "https://github.com/Brawl345/telegram-update-dumper-bot";
            license = pkgs.lib.licenses.unlicense;
            platforms = pkgs.lib.platforms.linux ++ pkgs.lib.platforms.darwin;
          };
        };

        default = self.packages.${pkgs.stdenv.hostPlatform.system}.telegram-update-dumper-bot;
      });
    };
}
