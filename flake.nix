{
  inputs = {
    flake-parts.url = "github:hercules-ci/flake-parts";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      imports = [ ];
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      perSystem = { lib, pkgs, ... }: {
        devShells.default = pkgs.mkShell {
          name = "ascii";
          buildInputs = with pkgs; [
            go
            pkg-config
            vhs
          ];
          nativeBuildInputs = with pkgs; [ mpv ];
          env.LD_LIBRARYPATH = lib.makeLibraryPath [ pkgs.mpv ];
          env.CGO_ENABLED = 1;
        };
        packages.default = pkgs.callPackage ./default.nix { };
      };
    };
}
