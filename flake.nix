{
  description = "autoprobe - AI-powered performance optimization";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_24  # Latest available in nixpkgs (1.26 not released yet)
          ];

          shellHook = ''
            echo "autoprobe dev shell"
            echo "Go version: $(go version)"
          '';
        };
      });
}
