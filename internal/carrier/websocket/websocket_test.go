package websocket

import (
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformanceWSS(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}

func TestConformanceWS(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{Plain: true, Path: "/custom"}), "127.0.0.1:0", []int{1, 1400})
}
