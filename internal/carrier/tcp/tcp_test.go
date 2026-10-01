package tcp

import (
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformance(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}
