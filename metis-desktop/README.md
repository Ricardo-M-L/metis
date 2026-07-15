# Metis Desktop

The native Metis client is built with Wails. Go owns the application backend,
session access, settings, and Metis CLI execution; the `frontend` directory is
the WebView user interface. Files under `frontend/wailsjs` are generated Wails
bindings for the exported Go methods.

## Run from the Metis CLI

Build the desktop application once, then launch it for the current workspace:

```sh
cd metis-desktop
wails build
cd ..
go build -o bin/metis ./cmd/metis
./bin/metis desktop
```

`metis desktop` launches the native application. The legacy browser UI is only
available through the explicit `metis desktop --web` option.

## Development

```sh
cd metis-desktop
wails dev
```

Wails development mode uses Vite for live frontend reloads. Normal Go tests do
not require `frontend/dist`; production builds generate and embed that directory
before compiling the application.

## Tests and production build

```sh
cd metis-desktop
go test ./...
go test -race ./...
cd frontend && npm run build && cd ..
wails build
```
