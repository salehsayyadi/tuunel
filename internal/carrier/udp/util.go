package udp

import (
	"os"
	"unsafe"
)

func getenv(k string) string { return os.Getenv(k) }

func ptr(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }
