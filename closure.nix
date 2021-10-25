{ flakes }:
let
  nixpkgs = builtins.getFlake "github:nixos/nixpkgs/nixos-21.05";
  inherit (nixpkgs.legacyPackages.x86_64-linux) lib buildPackages;
  inherit (builtins) match elemAt getFlake length fromJSON;

  resolve = flakeURI:
    let
      split = match "^([^#]+)#(.*)$" flakeURI;
      flake = elemAt split 0;
      attr = elemAt split 1;
      path = lib.splitString "." attr;
      root = getFlake flake;

      paths = [
        path
        ([ "packages" "x86_64-linux" ] ++ path)
        ([ "legacyPackages" "x86_64-linux" ] ++ path)
      ];

      findRoute = route:
        if (lib.hasAttrByPath route root) then
          lib.getAttrFromPath route root
        else
          null;

      notNull = e: e != null;
      allFound = lib.filter notNull (map findRoute paths);
    in if (length allFound) > 0 then
      elemAt allFound 0
    else
      throw "No attribute '${attr}' in flake '${flake}' found";

  drvs = map resolve (fromJSON flakes);
in buildPackages.closureInfo { rootPaths = drvs; }
