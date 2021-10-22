{
  description = "Flake for the nomad nix driver";

  inputs = {
    devshell.url = "github:numtide/devshell";
    inclusive.url = "github:input-output-hk/nix-inclusive";
    nixpkgs.url = "github:manveru/nixpkgs/use-atomic-bind-mounts";
    # nixpkgs.url = "path:/home/manveru/ghq/github.com/nixos/nixpkgs";
    utils.url = "github:kreisys/flake-utils";
  };

  outputs = { self, nixpkgs, utils, devshell, ... }@inputs:
    utils.lib.simpleFlake {
      systems = [ "x86_64-linux" ];
      inherit nixpkgs;

      preOverlays = [ devshell.overlay ];

      overlay = final: prev: {
        gocritic = prev.callPackage ./pkgs/gocritic.nix { };

        nomad-driver-nix = prev.buildGoModule rec {
          pname = "nomad-driver-nix";
          version = "2021.10.19.001";
          vendorSha256 = "sha256-FDJpbNtcFEHnZvWip2pvUHF3BFyfcSohrr/3nk9YS24=";

          src = inputs.inclusive.lib.inclusive ./. [
            ./nix
            ./go.mod
            ./go.sum
            ./main.go
          ];

          CGO_ENABLED = "0";
          GOOS = "linux";

          ldflags = [
            "-s"
            "-w"
            "-extldflags"
            "-static"
            "-X github.com/input-output-hk/nomad-driver-nix/nix.pluginVersion=${version}"
          ];

          postInstall = ''
            mv $out/bin/nomad-driver-nix $out/bin/nix-driver
          '';
        };

        example-rb = prev.writeScriptBin "example-rb" ''
          #!${prev.ruby}/bin/ruby

          require "fileutils"

          system("nix", "build", ".#nixosConfigurations.example.config.system.build.closure") ||
            (puts "Failed to build system closure"; exit 1)

          closure = File.readlink("./result")

          system("nix", "build", ".#nixosConfigurations.example.config.system.build.toplevel") ||
            (puts "Failed to build system configuration"; exit 1)

          result = File.readlink("./result")

          paths = `nix-store --query --requisites #{result}`.lines.map(&:strip).flatten.sort.uniq

          binds = paths.map{|path| "--bind-ro=#{path}" }
          binds << "--bind-ro=#{closure}/registration:/registration"
          binds << "--bind-ro=#{result}/init:/init"

          exec("sudo", "systemd-nspawn",
            *binds,
            "--setenv", "PATH=/sw/bin",
            "--volatile=overlay",
            # "--as-pid2", "#{result}/sw/bin/hostname"
            "/init"
          )
        '';

        example = prev.writeShellScriptBin "example" ''
          set -exuo pipefail

          sudo chattr -i testing/var/empty || true
          sudo rm -rf testing
          mkdir -p testing

          nix build .#nixosConfigurations.example.config.system.build.toplevel

          sudo systemd-nspawn \
            --bind-ro=/nix/store \
            --setenv PATH=/sw/bin \
            --directory ./testing \
            $(realpath ./result/init)
        '';
      };

      packages = { nomad-driver-nix, bash, coreutils, gocritic, example
        , example-rb }@pkgs:
        pkgs // {
          lib = nixpkgs.lib;
          defaultPackage = nomad-driver-nix;
        };

      extraOutputs.nixosConfigurations = {
        example = nixpkgs.lib.nixosSystem {
          system = "x86_64-linux";
          specialArgs.self = self;
          modules = [
            ({ config, lib, pkgs, ... }: {
              system.build.closure = pkgs.buildPackages.closureInfo {
                rootPaths = [ config.system.build.toplevel ];
              };

              boot.postBootCommands = ''
                # After booting, register the contents of the Nix store in the container in the Nix database in the tmpfs.
                ${config.nix.package.out}/bin/nix-store --load-db < /registration
                # nixos-rebuild also requires a "system" profile and an /etc/NIXOS tag.
                touch /etc/NIXOS
                ${config.nix.package.out}/bin/nix-env -p /nix/var/nix/profiles/system --set /run/current-system
              '';
            })
            { networking.hostName = "example"; }
            ./container.nix
          ];
        };
      };

      hydraJobs = { nomad-driver-nix }@pkgs: pkgs;

      devShell = { devshell }: devshell.fromTOML ./devshell.toml;
    };
}
