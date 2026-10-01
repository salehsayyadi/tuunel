package session

import "testing"

func BenchmarkSealOpen1400(b *testing.B) {
	a, _ := GenerateKeyPair()
	k, _ := GenerateKeyPair()
	resp := NewResponder(k, nil, func([]byte) bool { return true })
	in, m1, _ := Initiate(a, k.Public, nil, "b", false)
	sb, m2, _ := resp.Respond(m1)
	sa, _ := in.Finish(m2)
	body := make([]byte, 1400)
	buf := make([]byte, 0, 1500)
	b.SetBytes(1400)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg, _ := sa.Seal(buf[:0], InnerIP, body)
		if _, _, err := sb.Open(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHandshake(b *testing.B) {
	a, _ := GenerateKeyPair()
	k, _ := GenerateKeyPair()
	resp := NewResponder(k, nil, func([]byte) bool { return true })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		in, m1, _ := Initiate(a, k.Public, nil, "b", false)
		_, m2, err := resp.Respond(m1)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := in.Finish(m2); err != nil {
			b.Fatal(err)
		}
	}
}
