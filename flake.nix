{
  description = "auto-patcher fork of github/github-mcp-server";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    auto-patcher-skills = {
      url = "github:auto-patcher/skills";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      auto-patcher-skills,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        skills = auto-patcher-skills.lib.mkSkillsPackage pkgs;
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
          ];
          shellHook = ''
            mkdir -p .claude/skills
            for skill in ${skills}/share/claude/skills/*/SKILL.md; do
              name=$(basename $(dirname $skill))
              mkdir -p ".claude/skills/$name"
              ln -sfn "$skill" ".claude/skills/$name/SKILL.md"
            done
            echo "github-mcp-server (auto-patcher fork)"
          '';
        };
      }
    );
}
