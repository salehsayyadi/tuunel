//go:build linux

package icmp

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The kernel answers every echo request itself unless
// net.ipv4.icmp_echo_ignore_all=1, which would also break ordinary ping on
// the host. Instead the listener installs one narrow nftables rule in its own
// table that drops only kernel-generated echo replies whose payload begins
// with the tunnel request magic "TUNQ" (bytes 8..11 of the ICMP header +
// payload). Ordinary pings and all other host ICMP behaviour are untouched.
// Tunnel replies are sent with the magic "TUNR" and are not matched.

var nftPaths = []string{"nft", "/usr/sbin/nft", "/sbin/nft", "/usr/bin/nft"}

func nftBinary() (string, error) {
	for _, p := range nftPaths {
		if path, err := exec.LookPath(p); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("nft not found")
}

func nft(script string) error {
	bin, err := nftBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// FilterRuleset is the exact ruleset installed (documented in SECURITY.md).
func FilterRuleset() string {
	return fmt.Sprintf(`add table ip %[1]s
flush table ip %[1]s
table ip %[1]s {
	chain kernel_echo_reply {
		type filter hook output priority 0; policy accept;
		icmp type echo-reply @th,64,32 0x%08x counter drop comment "tuunel: drop kernel replies to tunnel requests"
	}
}
`, filterTable, uint32(magicReq[0])<<24|uint32(magicReq[1])<<16|uint32(magicReq[2])<<8|uint32(magicReq[3]))
}

func installReplyFilter() error { return nft(FilterRuleset()) }

func removeReplyFilter() error {
	return nft(fmt.Sprintf("delete table ip %s\n", filterTable))
}
