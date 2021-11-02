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
          version = "2021.11.01.002";
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
          export PATH="${final.nixUnstable}/bin:$PATH"
          ${final.nixUnstable}/bin/nix-store --load-db < /registration
          exec ${final.nixUnstable}/bin/nix "$@"
        '';

        nix-setup = prev.writeShellScript "nix-setup" ''
          export PATH="$PATH:${prev.git}/bin:${prev.nixUnstable}/bin"
          export SSL_CERT_FILE="${prev.cacert}/etc/ssl/certs/ca-bundle.crt"

          ${prev.coreutils}/bin/mkdir -p /etc
          echo 'nixbld:x:30000:nixbld1' > /etc/group
          echo 'nixbld1:x:30001:30000:Nix build user 1:/var/empty:${prev.shadow}/bin/nologin' > /etc/passwd
        '';
      };

      packages = { nomad-driver-nix, bash, coreutils, gocritic, wrap-nix }@pkgs:
        pkgs // {
          lib = nixpkgs.lib;
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
              environment.systemPackages = [ pkgs.wrap-nix ];

              nix = {
                package = pkgs.nixUnstable;
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
