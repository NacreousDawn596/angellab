{
  description = "AngelLab — Autonomous system guardians for Linux";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
  let
    systems = [ "x86_64-linux" "aarch64-linux" ];

    forAllSystems = f:
      nixpkgs.lib.genAttrs systems (system:
        f (import nixpkgs { inherit system; })
      );

  in {

    packages = forAllSystems (pkgs:
      let
        angellab = pkgs.buildGoModule {
          pname = "angellab";
          version = "0.1.0";

          CGO_ENABLED = 1;
          buildInputs = [ pkgs.sqlite ];

          src = self;

          vendorHash = null;

          subPackages = [
            "cmd/labd"
            "cmd/lab"
            "cmd/angel"
          ];

          ldflags = [
            "-s"
            "-w"
          ];
        };
      in {
        default = angellab;
      }
    );

    apps = forAllSystems (pkgs: {
      default = {
        type = "app";
        program = "${self.packages.${pkgs.system}.default}/bin/lab";
      };

      lab = {
        type = "app";
        program = "${self.packages.${pkgs.system}.default}/bin/lab";
      };

      labd = {
        type = "app";
        program = "${self.packages.${pkgs.system}.default}/bin/labd";
      };

      angel = {
        type = "app";
        program = "${self.packages.${pkgs.system}.default}/bin/angel";
      };
    });

    devShells = forAllSystems (pkgs: {
      default = pkgs.mkShell {
        buildInputs = [
          pkgs.go
          pkgs.gcc
          pkgs.sqlite
        ];
      };
    });

    nixosModules.default = { config, lib, pkgs, ... }:

      let
        cfg = config.services.angellab;
        pkg = self.packages.${pkgs.system}.default;
      in
      {
        options.services.angellab = {
          enable = lib.mkEnableOption "AngelLab daemon";

          package = lib.mkOption {
            type = lib.types.package;
            default = pkg;
          };

          configFile = lib.mkOption {
            type = lib.types.path;
            default = ./configs/angellab.toml;
          };
        };

        config = lib.mkIf cfg.enable {

          users.groups.angellab = {};

          users.users.angellab = {
            isSystemUser = true;
            group = "angellab";
            description = "AngelLab daemon";
            home = "/var/lib/angellab";
            createHome = false;
            shell = pkgs.shadow.nologin;
          };

          environment.systemPackages = [
            cfg.package
          ];

          systemd.tmpfiles.rules = [
            "d /run/angellab 0755 root root -"
            "d /var/lib/angellab 0750 angellab angellab -"
            "d /var/lib/angellab/snapshots 0700 angellab angellab -"
            "d /var/log/angellab 0750 angellab angellab -"
            "d /etc/angellab 0750 root angellab -"
          ];

          environment.etc."angellab/angellab.toml".source = cfg.configFile;

          systemd.services.angellab = {
            description = "AngelLab daemon";
            after = [ "network.target" ];
            wantedBy = [ "multi-user.target" ];

            serviceConfig = {
              User = "angellab";
              Group = "angellab";

              ExecStart = "${cfg.package}/bin/labd";

              Restart = "always";
              RestartSec = 3;

              RuntimeDirectory = "angellab";

              StateDirectory = "angellab";
              LogsDirectory = "angellab";

              NoNewPrivileges = true;
              PrivateTmp = true;
              ProtectSystem = "strict";
              ProtectHome = true;
            };
          };

        };
      };

  };
}