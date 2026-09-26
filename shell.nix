{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  # nativeBuildInputs are build tools
  nativeBuildInputs = with pkgs; [
    pkg-config
    go
    gcc
  ];

  # buildInputs are the C libraries your project links against
  buildInputs = with pkgs; [
    portaudio
  ];
}
