// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command start is the users service the Fiber migration guide starts from: an API of users on Fiber.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofiber/fiber/v3"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":3000"
	}

	app := newServer(newStore())
	if err := app.Listen(addr, fiber.ListenConfig{GracefulContext: ctx}); err != nil {
		log.Fatal(err)
	}
}
