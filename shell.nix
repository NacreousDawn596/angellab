{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  name = "angellab-dev";

  buildInputs = with pkgs; [
    go
    gcc
    pkg-config
    sqlite
    gnumake
    git
  ];

  CGO_ENABLED = "1";

  shellHook = ''
    echo "AngelLab development shell"
    echo
    echo "Host requirements:"
    echo "  Linux kernel >= 4.18"
    echo "  /proc mounted"
    echo "  inotify available"
    echo "  cgroup v2 optional"
    echo
    go version
  '';
}