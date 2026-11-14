// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command start is the users service the Echo migration guide starts from: an API of users on Echo.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	e := newServer(newStore())
	sc := echo.StartConfig{Address: addr, GracefulTimeout: 10 * time.Second}
	if err := sc.Start(ctx, e); err != nil {
		e.Logger.Error("failed to start server", "error", err)
	}
}
