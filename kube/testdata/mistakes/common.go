// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

// Package mistakes holds code that must not compile. mistakes_test.go builds
// common.go with each other file, one at a time, and checks that each one
// fails, except ok.go, which shows that common.go compiles.
package mistakes

type MemcachedSpec struct {
	Size  int32  `json:"size"`
	Image string `json:"image"`
}

var spec MemcachedSpec
