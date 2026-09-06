//go:build windows

package main

import (
	"context"

	"github.com/olibar/hpscan/internal/config"
	"github.com/olibar/hpscan/internal/service"
)

// runPlatform runs the daemon under the Service Control Manager when started
// by it, otherwise in the foreground console.
func runPlatform(cfg config.Config, body func(ctx context.Context) error) error {
	handled, err := service.RunAsWindowsService(body)
	if handled {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	return body(ctx)
}
