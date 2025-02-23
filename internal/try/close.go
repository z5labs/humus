// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package try

import (
	"errors"
	"io"
)

func Close(err *error, c io.Closer) {
	cerr := c.Close()
	if cerr == nil {
		return
	}
	if *err == nil {
		*err = cerr
		return
	}
	*err = errors.Join(*err, cerr)
}
