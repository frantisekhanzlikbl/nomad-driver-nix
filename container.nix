{ lib, pkgs, self, config, ... }: {
  imports = [
    (self.inputs.nixpkgs + /nixos/modules/profiles/headless.nix)
    (self.inputs.nixpkgs + /nixos/modules/profiles/minimal.nix)
    (self.inputs.nixpkgs + /nixos/modules/misc/version.nix)
  ];

  boot.isContainer = true;
  networking.useDHCP = false;
  networking.hostName = lib.mkDefault "nixos";

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

  # services.getty.autologinUser = "nixos";
}
