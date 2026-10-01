//go:build !linux

package icmp

import "errors"

func installReplyFilter() error { return errors.New("icmp reply filter: only supported on Linux") }
func removeReplyFilter() error  { return nil }
