{ path }:
let
  nixpkgs = builtins.getFlake "github:nixos/nixpkgs/nixos-21.05";
  inherit (nixpkgs.legacyPackages.x86_64-linux) buildPackages;
in buildPackages.closureInfo { rootPaths = builtins.storePath path; }
