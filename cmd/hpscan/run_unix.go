//go:build !windows

package main

import (
	"context"

	"github.com/olibar/hpscan/internal/config"
)

// runPlatform runs the daemon in the foreground on Unix-like systems.
func runPlatform(cfg config.Config, body func(ctx context.Context) error) error {
	ctx, cancel := signalContext()
	defer cancel()
	return body(ctx)
}
