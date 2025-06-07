// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	bedrockcfg "github.com/z5labs/bedrock/config"
)

func TestConfig_InitializeOTel(t *testing.T) {
	t.Run("will not return an error", func(t *testing.T) {
		t.Run("with the default parameters", func(t *testing.T) {
			m, err := bedrockcfg.Read(DefaultConfig())
			require.Nil(t, err)

			var cfg Config
			err = m.Unmarshal(&cfg)
			require.Nil(t, err)

			err = cfg.InitializeOTel(context.Background())
			require.Nil(t, err)
		})
	})
}
