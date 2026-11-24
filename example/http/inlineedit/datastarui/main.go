// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command datastarui serves inlineedit's contacts with Datastar: a row edited in place, and a live feed of every saved row.
package main

import "github.com/z5labs/humus/http/httpserver"

func main() {
	httpserver.Main(Init)
}
