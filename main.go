package main

import (
	"embed"
	"fmt"
	"os"

	"crazyget/core"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--selftest" || os.Args[1] == "-t") {
		os.Exit(core.SelfTest())
	}

	m, err := core.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败:", err)
		os.Exit(1)
	}
	defer m.Close()

	app := NewApp(m)

	err = wails.Run(&options.App{
		Title:            "CrazyGet - 多协议下载器",
		Width:            980,
		Height:           660,
		MinWidth:         780,
		MinHeight:        520,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 32, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.Dark,
			CustomTheme: &windows.ThemeSettings{
				DarkModeTitleBar:   windows.RGB(24, 26, 32),
				DarkModeTitleText:  windows.RGB(232, 234, 237),
				LightModeTitleBar:  windows.RGB(245, 246, 250),
				LightModeTitleText: windows.RGB(32, 33, 36),
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
