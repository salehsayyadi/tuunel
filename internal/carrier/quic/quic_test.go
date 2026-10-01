package quic

import (
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformanceStream(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}

func TestConformanceDatagram(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{Datagrams: true}), "127.0.0.1:0", []int{1, 100, 1000, DatagramMax})
}
