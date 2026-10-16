// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"errors"
	"testing"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
)

func TestReadSettings(t *testing.T) {
	t.Run("defaults the grace period to 10s", func(t *testing.T) {
		s, err := humus.ReadSettings(t.Context(), bedrock.NewStaticEnvironment(nil))
		if err != nil {
			t.Fatal(err)
		}
		if s.ShutdownGracePeriod != 10*time.Second {
			t.Errorf("ShutdownGracePeriod = %s, want 10s", s.ShutdownGracePeriod)
		}
	})

	t.Run("reads HUMUS_SHUTDOWN_GRACE_PERIOD", func(t *testing.T) {
		env := bedrock.NewStaticEnvironment(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "1m30s"})
		s, err := humus.ReadSettings(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		if s.ShutdownGracePeriod != 90*time.Second {
			t.Errorf("ShutdownGracePeriod = %s, want 1m30s", s.ShutdownGracePeriod)
		}
	})

	for _, value := range []string{"30", "", "soon", "0s", "-5s"} {
		t.Run("rejects "+value, func(t *testing.T) {
			env := bedrock.NewStaticEnvironment(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": value})
			_, err := humus.ReadSettings(t.Context(), env)
			cerr, ok := errors.AsType[*config.Error](err)
			if !ok {
				t.Fatalf("err = %v, want a *config.Error", err)
			}
			if cerr.Name != "HUMUS_SHUTDOWN_GRACE_PERIOD" {
				t.Errorf("config.Error.Name = %q, want HUMUS_SHUTDOWN_GRACE_PERIOD", cerr.Name)
			}
		})
	}
}
