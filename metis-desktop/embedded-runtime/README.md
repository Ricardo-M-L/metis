# Generated Desktop CLI payload

`python3 scripts/prepare-desktop-cli.py --platform linux/amd64` (or a Windows
target) builds the matching CLI from this checkout before the Wails production
build. Generated target directories contain `metis[.exe]` and `manifest.json`.
Production embeds these bytes inside the Desktop executable; users do not need
a separate CLI installation or a second file in the release archive.

For macOS, run the script after Wails with `--platform darwin/universal --app
metis-desktop/build/bin/METIS.app`. The CLI and manifest reside in
`Contents/Resources/bin`. Sign the CLI before hashing it, then sign the outer app
without re-signing nested code. Missing, altered or incompatible payloads are
errors in a production Desktop; this README only keeps a clean production build
compilable for diagnostics. Never add credentials or generated binaries here.
