package main

import (
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

func main() {
	app := NewApp()

	err := wails.Run(desktopOptions(app))

	if err != nil {
		println("Error:", err.Error())
	}
}

func desktopOptions(app *App) *options.App {
	return &options.App{
		Title:     "Metis",
		Width:     1200,
		Height:    800,
		MinWidth:  800,
		MinHeight: 600,
		Frameless: false,
		// Wails v2 defaults the native macOS zoom flag to false when Mac is
		// nil, which disables the green traffic-light button even though the
		// window itself remains resizable. Keep the native maximize affordance
		// explicit on macOS while retaining the cross-platform defaults.
		Mac: &mac.Options{DisableZoom: false},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 26, G: 26, B: 26, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	}
}
