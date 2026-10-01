package icmp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestDisabledByDefault(t *testing.T) {
	err := New(carrier.Options{}).Check(context.Background())
	if !errors.Is(err, carrier.ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestPermissionDetection(t *testing.T) {
	err := New(carrier.Options{Experimental: true}).Check(context.Background())
	if err == nil {
		t.Skip("running with CAP_NET_RAW")
	}
	if !errors.Is(err, carrier.ErrPermission) {
		t.Fatalf("want ErrPermission, got %v", err)
	}
}

func TestMarshalParse(t *testing.T) {
	m, err := marshal(8, 7, 9, magicReq, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	typ, e, ok := parse(m)
	if !ok || typ != 8 || e.ID != 7 || e.Seq != 9 || string(e.Data[4:]) != "hello" {
		t.Fatal("round trip")
	}
	if _, _, ok := parse([]byte{8, 0}); ok {
		t.Fatal("parsed garbage")
	}
}

// TestConformancePrivileged runs over loopback when CAP_NET_RAW is available
// (e.g. `sudo go test` or the lab script).
func TestConformancePrivileged(t *testing.T) {
	if os.Getenv("TUUNEL_PRIVILEGED") == "" {
		t.Skip("set TUUNEL_PRIVILEGED=1 and run as root to test raw ICMP")
	}
	carriertest.Run(t, New(carrier.Options{Experimental: true}), "127.0.0.1", []int{1, 100, 1000, MaxPayload})
}

func TestClassify(t *testing.T) {
	if !errors.Is(classify(os.ErrPermission), carrier.ErrPermission) {
		t.Fatal("EPERM not classified as permission error")
	}
	if !errors.Is(classify(errors.New("x")), carrier.ErrUnavailable) {
		t.Fatal("other errors must be unavailable")
	}
}
