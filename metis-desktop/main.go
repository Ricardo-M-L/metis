package main

import (
	"fmt"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"os"
)

func main() {
	if handled, err := desktopUpdateHelperCommand(os.Args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if handled, err := desktopRuntimeCheckCommand(os.Args, os.Stdout, nil, nil); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "Metis Desktop runtime check:", err)
			os.Exit(1)
		}
		return
	}
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
		// Keep native traffic lights and resizing while the macOS content
		// extends behind the transparent titlebar. Other platforms retain
		// their native framed window and receive no overlay layout inset.
		Mac: &mac.Options{
			TitleBar:    mac.TitleBarHiddenInset(),
			DisableZoom: false,
		},
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
