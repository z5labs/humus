// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"errors"
	"io"
)

// joinClose is only expected to be called from defer statements
func joinClose(err *error, c io.Closer) {
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
