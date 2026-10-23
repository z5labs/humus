// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"fmt"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/job"
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	name, err := config.Env(s.Process.Environment, "HELLO_NAME").Read(ctx)
	if err != nil {
		return nil, err
	}

	out := s.Process.Stdout
	greet := func(context.Context) error {
		_, err := fmt.Fprintf(out, "Hello, %s!\n", name)
		return err
	}
	return []humus.Service{job.NewService(s, greet)}, nil
}
