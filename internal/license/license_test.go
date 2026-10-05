package license

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type env struct {
	priv    ed25519.PrivateKey
	dir     string
	machine string
}

func setup(t *testing.T) *env {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldM, oldF, oldC := pubMarker, File, ClockFile
	t.Cleanup(func() { pubMarker, File, ClockFile = oldM, oldF, oldC })
	pubMarker = tag + base64.StdEncoding.EncodeToString(pub)
	dir := t.TempDir()
	File = filepath.Join(dir, "license")
	ClockFile = filepath.Join(dir, "clock")
	mid := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(mid, []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUUNEL_MACHINE_ID_FILE", mid)
	m, err := Machine()
	if err != nil {
		t.Fatal(err)
	}
	return &env{priv: priv, dir: dir, machine: m}
}

func (e *env) sign(p Payload) string {
	b, _ := json.Marshal(p)
	body := "TUL1." + base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(e.priv, []byte(body)))
}

func (e *env) install(t *testing.T, tok string) {
	t.Helper()
	if err := os.WriteFile(File, []byte(tok+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestUnlicensedBuild(t *testing.T) {
	if Required() || PublicKey() != nil {
		t.Fatal("placeholder build must not require a license")
	}
	if !Check().Valid {
		t.Fatal("unlicensed build must always be valid")
	}
}

func TestCheck(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	if st := Check(); st.Valid {
		t.Fatal("valid without a license file")
	}
	e.install(t, e.sign(Payload{ID: 1, Machine: e.machine, Role: "edge", Issued: now, Expires: now + 3600}))
	if st := Check(); !st.Valid || st.Payload.ID != 1 {
		t.Fatalf("want valid, got %+v", st)
	}
	e.install(t, e.sign(Payload{ID: 1, Machine: "ffffffffffffffffffffffffffffffff", Issued: now, Expires: now + 3600}))
	if st := Check(); st.Valid || !strings.Contains(st.Reason, "another server") {
		t.Fatalf("foreign machine accepted: %+v", st)
	}
	e.install(t, e.sign(Payload{ID: 1, Machine: e.machine, Issued: now - 7200, Expires: now - 3600}))
	if st := Check(); st.Valid || !strings.Contains(st.Reason, "expired") {
		t.Fatalf("expired accepted: %+v", st)
	}
	// tampered payload / foreign signer
	tok := e.sign(Payload{ID: 1, Machine: e.machine, Issued: now, Expires: now + 3600})
	parts := strings.Split(tok, ".")
	b, _ := json.Marshal(Payload{ID: 1, Machine: e.machine, Issued: now, Expires: now + 99999999})
	e.install(t, parts[0]+"."+base64.RawURLEncoding.EncodeToString(b)+"."+parts[2])
	if st := Check(); st.Valid {
		t.Fatal("tampered license accepted")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	e2 := &env{priv: other, machine: e.machine}
	e.install(t, e2.sign(Payload{ID: 1, Machine: e.machine, Issued: now, Expires: now + 3600}))
	if st := Check(); st.Valid {
		t.Fatal("license from another signer accepted")
	}
}

func TestClockRollback(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	e.install(t, e.sign(Payload{ID: 1, Machine: e.machine, Issued: now, Expires: now + 3600}))
	RecordServerTime(now + 7200) // the server has seen a later time than our clock
	if st := Check(); st.Valid {
		t.Fatal("rolled-back clock extended the license")
	}
}

func TestDamagedMarkerFailsClosed(t *testing.T) {
	setup(t)
	pubMarker = tag + "not-a-key-but-not-placeholder-either-xxxxxxx"
	if !Required() || Check().Valid {
		t.Fatal("damaged marker must fail closed")
	}
}

func TestSync(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	state := "ok"
	peer := "so+Mr/SthEnXD/gFbTO0OaBBKd8xBPtZI1MzBLfeDB0="
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(b))
		if r.URL.Path != "/api/check" || f.Get("machine") != e.machine || f.Get("license") == "" {
			fmt.Fprintln(w, "ERR bad request")
			return
		}
		switch state {
		case "revoked":
			fmt.Fprintln(w, "ERR revoked: gone")
		case "expired":
			fmt.Fprintln(w, "ERR expired: over")
		default:
			fmt.Fprintf(w, "OK\nLICENSE=%s\nPEER_KEY=%s\n", e.sign(Payload{ID: 7, Machine: e.machine, Role: "edge", Issued: now, Expires: now + 7200, Server: "http://" + r.Host}), peer)
		}
	}))
	defer srv.Close()
	srvURL := strings.Replace(srv.URL, "localhost", "127.0.0.1", 1)
	e.install(t, e.sign(Payload{ID: 7, Machine: e.machine, Role: "edge", Issued: now, Expires: now + 60, Server: srvURL}))
	cfg := filepath.Join(e.dir, "config.yaml")
	os.WriteFile(cfg, []byte("peers:\n  - {name: remote, public_key: \"REPLACE_ME_WITH_PEER_PUBLIC_KEY\", allowed_ips: [\"10.200.0.2/32\"]}\n"), 0o640)

	res, err := Sync(context.Background(), cfg)
	if err != nil || res.Revoked {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if c, _ := os.ReadFile(cfg); !strings.Contains(string(c), peer) {
		t.Fatalf("peer key not installed:\n%s", c)
	}
	if st := Check(); !st.Valid || st.Left < time.Hour {
		t.Fatalf("license not refreshed: %+v", st)
	}
	for _, s := range []string{"revoked", "expired"} {
		state = s
		if res, _ := Sync(context.Background(), cfg); !res.Revoked {
			t.Fatalf("%s: not reported", s)
		}
		if Check().Valid {
			t.Fatalf("%s: license still valid", s)
		}
		state = "ok"
		if res, err := Sync(context.Background(), cfg); err != nil || res.Revoked {
			t.Fatalf("%s: reinstatement failed: %+v %v", s, res, err)
		}
		if !Check().Valid {
			t.Fatalf("%s: license not restored", s)
		}
	}
}

func TestWatchStops(t *testing.T) {
	e := setup(t)
	old := CheckInterval
	CheckInterval = 20 * time.Millisecond
	defer func() { CheckInterval = old }()
	now := time.Now().Unix()
	e.install(t, e.sign(Payload{ID: 1, Machine: e.machine, Issued: now, Expires: now + 3600}))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Wait(context.Background(), log); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go Watch(context.Background(), log, func(err error) { done <- err })
	os.Remove(File)
	select {
	case err := <-done:
		if err != ErrLicense {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop the tunnel")
	}
}
