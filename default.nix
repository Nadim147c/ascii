{
  lib,
  mpv,
  buildGoModule,
  nix-update-script,
}:

buildGoModule (finalAttrs: {
  pname = "ascii";
  version = "0-unstable-2026-09-28";
  __structuredAttrs = true;

  src = lib.cleanSource (
    lib.fileset.toSource {
      root = ./.;
      fileset = lib.fileset.unions [
        ./bitmaps
        ./bitmaps.go
        ./main.go
        ./go.mod
        ./go.sum
      ];
    }
  );

  buildInputs = [ mpv ];

  vendorHash = "sha256-XlW53fqmFuwubBYJuoqrNhamLhvdJtbmaSvbDHQOBzw=";

  excludedPackages = [ "bitmaps" ];
  ldflags = [ "-s" ];

  passthru.updateScript = nix-update-script { };

  meta = {
    description = "Terminal ASCII images/videos in renderer";
    homepage = "https://github.com/Nadim147c/ascii";
    license = lib.licenses.gpl3Only;
    maintainers = with lib.maintainers; [ Nadim147c ];
    mainProgram = "ascii";
  };
})
