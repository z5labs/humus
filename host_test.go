// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"net"
	"reflect"
	"testing"

	"github.com/z5labs/bedrock/host"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
)

// Both filesystems bedrock's host module has are a humus.FS: this fails to
// compile if FS lists a method either lacks, or if FS stops being sealed by
// host.FS in a way that shuts either of them out.
var (
	_ humus.FS = host.OSFS{}
	_ humus.FS = (*hosttest.FS)(nil)
)

func TestOSHost_is_host_OS(t *testing.T) {
	h := humus.OSHost()

	if got, want := reflect.ValueOf(h.Listen).Pointer(), reflect.ValueOf(net.Listen).Pointer(); got != want {
		t.Error("Listen is not net.Listen")
	}
	if got, want := reflect.ValueOf(h.ListenPacket).Pointer(), reflect.ValueOf(net.ListenPacket).Pointer(); got != want {
		t.Error("ListenPacket is not net.ListenPacket")
	}
	if h.Dial == nil {
		t.Error("Dial is nil")
	}
	if fsys, ok := h.FS.(host.OSFS); !ok || fsys != (host.OSFS{}) {
		t.Errorf("FS is %#v, want host.OSFS{}", h.FS)
	}
}
