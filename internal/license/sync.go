package license

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/salehsayyadi/tuunel/internal/underlay"
)

// Response is a parsed license-server reply: first line "OK" or
// "ERR <message>", then KEY=VALUE lines (the installer parses the same
// format in shell).
type Response struct {
	OK     bool
	Error  string
	Values map[string]string
}

// ParseResponse parses a server reply.
func ParseResponse(body string) Response {
	r := Response{Values: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if line == "OK" {
				r.OK = true
			} else {
				r.Error = strings.TrimSpace(strings.TrimPrefix(line, "ERR"))
				if r.Error == "" {
					r.Error = "unexpected server reply"
				}
			}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			r.Values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return r
}

// Post sends form values to server+path.
func Post(ctx context.Context, server, path string, form url.Values) (Response, error) {
	if !strings.HasPrefix(server, "https://") && !strings.HasPrefix(server, "http://127.0.0.1") {
		return Response{}, fmt.Errorf("license: server must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r := ParseResponse(string(b))
	if !r.OK && r.Error == "unexpected server reply" && resp.StatusCode != 200 {
		r.Error = fmt.Sprintf("server HTTP %d", resp.StatusCode)
	}
	return r, nil
}

var keyRe = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// SyncResult reports what Sync did.
type SyncResult struct {
	Refreshed   bool
	Revoked     bool
	PeerUpdated bool
	Message     string
}

// Sync refreshes the license online and, on the edge, installs the paired
// remote's public key into the configuration. It must run as root. The
// caller restarts the service when PeerUpdated or when a license became
// valid again.
func Sync(ctx context.Context, configPath string) (SyncResult, error) {
	var res SyncResult
	b, err := os.ReadFile(File)
	revokedFile := false
	if err != nil {
		// a revoked license is kept aside so re-activation by the admin can restore it
		if b2, err2 := os.ReadFile(File + ".revoked"); err2 == nil {
			b, err, revokedFile = b2, nil, true
		} else {
			return res, fmt.Errorf("license: %w", err)
		}
	}
	token := firstLine(string(b))
	p, err := Parse(token, PublicKey())
	if err != nil {
		return res, err
	}
	m, err := Machine()
	if err != nil {
		return res, err
	}
	pub, _ := os.ReadFile(filepath.Join(filepath.Dir(File), "node.pub"))
	src := sourceTowardsPeer(configPath)
	form := url.Values{"license": {token}, "machine": {m}, "pubkey": {strings.TrimSpace(string(pub))}, "src": {src}}
	r, err := Post(ctx, p.Server, "/api/check", form)
	if err != nil {
		return res, fmt.Errorf("license server unreachable: %w", err)
	}
	if !r.OK {
		if strings.HasPrefix(r.Error, "revoked") || strings.HasPrefix(r.Error, "deleted") || strings.HasPrefix(r.Error, "expired") {
			if !revokedFile {
				_ = os.Rename(File, File+".revoked")
			}
			res.Revoked = true
		}
		res.Message = r.Error
		return res, nil
	}
	if revokedFile { // reinstated by the admin
		if err := writeAtomic(File, token+"\n", 0o640); err != nil {
			return res, err
		}
		if st, err := os.Stat(File + ".revoked"); err == nil {
			if o, ok := statOwner(st); ok {
				_ = os.Chown(File, o.uid, o.gid)
			}
		}
		_ = os.Remove(File + ".revoked")
	}
	if nt := r.Values["LICENSE"]; nt != "" && nt != token {
		np, err := Parse(nt, PublicKey())
		if err == nil && np.Machine == m {
			if err := writeAtomic(File, nt+"\n", 0o640); err != nil {
				return res, err
			}
			res.Refreshed = true
			RecordServerTime(np.Issued)
		}
	} else {
		RecordServerTime(p.Issued)
	}
	if pk := r.Values["PEER_KEY"]; keyRe.MatchString(pk) && configPath != "" {
		changed, err := setPeerKey(configPath, pk)
		if err != nil {
			return res, err
		}
		res.PeerUpdated = changed
	}
	if cv := r.Values["CFG"]; cv != "" && configPath != "" {
		changed, err := applyTags(configPath, parseTags(cv))
		if err != nil {
			return res, err
		}
		if changed {
			res.PeerUpdated = true
			// tell the server our underlay source address right away
			if s2 := sourceTowardsPeer(configPath); s2 != "" && s2 != src {
				form.Set("src", s2)
				_, _ = Post(ctx, p.Server, "/api/check", form)
			}
		}
	}
	return res, nil
}

// Configuration values the license server may set are marked in the YAML
// with a trailing "# tuunel:<tag>" comment (written by the licensed installer).
var (
	tagLineRe = regexp.MustCompile(`(?m)^([^#\n]*?:[ \t]*)("?)([^"\s#]*)("?)([ \t]+#[ \t]*tuunel:([a-z_]+)\b[^\n]*)$`)
	tagNameRe = regexp.MustCompile(`^[a-z_]{1,24}$`)
	tagValRe  = regexp.MustCompile(`^[0-9A-Za-z.:\-]{1,64}$`)
)

// parseTags parses "tag:value,tag:value" (values may contain no commas).
func parseTags(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), ":")
		if ok && tagNameRe.MatchString(k) && tagValRe.MatchString(v) {
			out[k] = v
		}
	}
	return out
}

// configTags returns the current tagged values of a configuration file.
func configTags(text string) map[string]string {
	out := map[string]string{}
	for _, m := range tagLineRe.FindAllStringSubmatch(text, -1) {
		out[m[6]] = m[3]
	}
	return out
}

func applyTags(path string, vals map[string]string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	text := string(b)
	nt := tagLineRe.ReplaceAllStringFunc(text, func(line string) string {
		m := tagLineRe.FindStringSubmatch(line)
		v, ok := vals[m[6]]
		if !ok || v == m[3] {
			return line
		}
		return m[1] + m[2] + v + m[4] + m[5]
	})
	if nt == text {
		return false, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	_ = os.WriteFile(path+".bak.license", b, st.Mode().Perm())
	return true, writeAtomic(path, nt, st.Mode().Perm())
}

// sourceTowardsPeer returns this host's IPv4 towards the paired peer
// (tag remote_addr), which the peer needs as its GRE remote.
func sourceTowardsPeer(configPath string) string {
	if configPath == "" {
		return ""
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	a := configTags(string(b))["remote_addr"]
	if a == "" || a == "0.0.0.0" {
		return ""
	}
	return underlay.SourceFor(nil, a)
}

// setPeerKey replaces the (single) peer public key in the configuration if
// it differs. Only the first public_key occurrence is touched.
func setPeerKey(path, key string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	re := regexp.MustCompile(`public_key:\s*"([^"]*)"`)
	loc := re.FindSubmatchIndex(b)
	if loc == nil {
		return false, nil
	}
	if string(b[loc[2]:loc[3]]) == key {
		return false, nil
	}
	nb := append(append(append([]byte{}, b[:loc[2]]...), key...), b[loc[3]:]...)
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	_ = os.WriteFile(path+".bak.license", b, st.Mode().Perm())
	if err := writeAtomic(path, string(nb), st.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func writeAtomic(path, data string, mode os.FileMode) error {
	st, statErr := os.Stat(path)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), mode); err != nil {
		return err
	}
	if statErr == nil {
		if sys, ok := statOwner(st); ok {
			_ = os.Chown(tmp, sys.uid, sys.gid)
		}
		_ = os.Chmod(tmp, st.Mode().Perm())
	}
	return os.Rename(tmp, path)
}
