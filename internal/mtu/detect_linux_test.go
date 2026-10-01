//go:build linux

package mtu

import "testing"

func TestDetectLoopback(t *testing.T) {
	m, err := DetectPathMTU("127.0.0.1:9")
	if err != nil {
		t.Skip(err)
	}
	if m < 1280 {
		t.Fatalf("implausible loopback MTU %d", m)
	}
}
