//go:build darwin

package main

import (
	"github.com/getlantern/systray"
	"github.com/r1chjames/sftp-sync/internal/apiclient"
)

func main() {
	systray.Run(onReady, onExit)
}

var manager *DaemonManager

func onReady() {
	systray.SetTemplateIcon(iconData, iconData)
	systray.SetTooltip("sftpsync")

	client := apiclient.New()
	manager = NewDaemonManager(client)
	menu := buildMenu()
	r := newRefresher()

	// Start polling; update menu on each result.
	r.start(client, menu.update)

	// Start the daemon if it isn't already running, then trigger an immediate refresh.
	go func() {
		manager.EnsureRunning()
		r.now()
	}()

	// Handle menu clicks.
	go menu.eventLoop(r, client, manager)
}

func onExit() {
	manager.Shutdown()
}
