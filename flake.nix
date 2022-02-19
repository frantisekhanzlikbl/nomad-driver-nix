{
  description = "Flake for the nomad nix driver";

  inputs = {
    devshell.url = "github:numtide/devshell";
    inclusive.url = "github:input-output-hk/nix-inclusive";
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    nix.url = "github:nixos/nix";
    utils.url = "github:kreisys/flake-utils";
  };

  outputs = { self, nixpkgs, utils, devshell, nix, ... }@inputs:
    utils.lib.simpleFlake {
      systems = [ "x86_64-linux" ];
      inherit nixpkgs;

      preOverlays = [
        devshell.overlay
        nix.overlay
      ];

      overlay = final: prev: {
        gocritic = prev.callPackage ./pkgs/gocritic.nix { };

        nomad-driver-nix = prev.buildGoModule rec {
          pname = "nomad-driver-nix";
          version = "2022.02.19.001";
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

        wrap-nix = prev.writeShellScriptBin "nix" ''
          set -exuo pipefail

          export PATH="$PATH:${prev.git}/bin:${prev.nix}/bin:${prev.coreutils}/bin"
          export SSL_CERT_FILE="${prev.cacert}/etc/ssl/certs/ca-bundle.crt"

          if ! id nixbld1 &> /dev/null; then
              mkdir -p /etc
              echo 'nixbld:x:30000:nixbld1' > /etc/group
              echo 'nixbld1:x:30001:30000:Nix build user 1:/var/empty:${prev.shadow}/bin/nologin' > /etc/passwd
              nix-store --load-db < /registration
          fi

          exec ${prev.nix}/bin/nix "$@"
        '' // {
          inherit (prev.nix) version;
        };
      };

      packages = { nomad-driver-nix, bash, coreutils, gocritic, wrap-nix }@pkgs:
        pkgs // {
          inherit (nixpkgs) lib;
          defaultPackage = nomad-driver-nix;
        };

      extraOutputs.nixosModules = {
        nix-driver-nomad = { pkgs, config, lib, ... }: {
          system.build.closure = pkgs.buildPackages.closureInfo {
            rootPaths = [ config.system.build.toplevel ];
          };

          boot.postBootCommands = lib.mkDefault ''
            # After booting, register the contents of the Nix store in the container in the Nix database in the tmpfs.
            ${config.nix.package.out}/bin/nix-store --load-db < /registration
            # nixos-rebuild also requires a "system" profile and an /etc/NIXOS tag.
            touch /etc/NIXOS
            ${config.nix.package.out}/bin/nix-env -p /nix/var/nix/profiles/system --set /run/current-system
          '';

          systemd.services.console-getty.enable = false;

          # Log everything to the serial console.
          services.journald.extraConfig = ''
            ForwardToConsole=yes
            MaxLevelConsole=debug
          '';

          systemd.extraConfig = ''
            # Don't clobber the console with duplicate systemd messages.
            ShowStatus=no
            # Allow very slow start
            DefaultTimeoutStartSec=300
          '';

          boot.isContainer = lib.mkDefault true;
          networking.useDHCP = lib.mkDefault false;
        };
      };

      extraOutputs.nixosConfigurations = {
        example = nixpkgs.lib.nixosSystem {
          system = "x86_64-linux";
          specialArgs.self = self;
          modules = [
            self.outputs.nixosModules.nix-driver-nomad
            (nixpkgs + /nixos/modules/profiles/headless.nix)
            (nixpkgs + /nixos/modules/profiles/minimal.nix)
            (nixpkgs + /nixos/modules/misc/version.nix)
            ({ lib, pkgs, self, config, ... }: {
              nixpkgs.overlays = [ self.overlay ];
              networking.hostName = lib.mkDefault "example";

              nix = {
                systemFeatures = [ "recursive-nix" "nixos-test" ];
                extraOptions = ''
                  experimental-features = nix-command flakes ca-references recursive-nix
                '';
              };

              users.users = {
                nixos = {
                  isNormalUser = true;
                  extraGroups = [ "wheel" ];
                  initialHashedPassword = "";
                };

                root.initialHashedPassword = "";
              };

              security.sudo = {
                enable = lib.mkDefault true;
                wheelNeedsPassword = lib.mkForce false;
              };
            })
          ];
        };
      };

      hydraJobs = { nomad-driver-nix }@pkgs: pkgs;

      devShell = { devshell }: devshell.fromTOML ./devshell.toml;
    };
}
