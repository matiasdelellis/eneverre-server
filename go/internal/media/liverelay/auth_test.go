package liverelay

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// A reader with a bad path token is reported (for the security log) with the
// camera id only; a valid token is not, and neither is the token logged.
func TestRelayReportsRejectedPathTokens(t *testing.T) {
	addr := freeAddr(t)
	var mu sync.Mutex
	var reports []string
	r := &Relay{
		Address: addr,
		Authorize: func(path string) (string, bool) {
			cam, token, ok := strings.Cut(path, "/")
			return cam, ok && token == "good"
		},
		OnAuthFailure: func(ip, user, path string) {
			mu.Lock()
			reports = append(reports, ip+"|"+user+"|"+path)
			mu.Unlock()
		},
	}
	if err := r.Initialize(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	describe := func(path string) {
		u, err := base.ParseURL("rtsp://" + addr + "/" + path)
		if err != nil {
			t.Fatal(err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _, _ = c.Describe(u) // 401 or 404 (no source) — both fine here
	}
	describe("cam1/good")
	describe("cam1/bad")

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || reports[0] != "127.0.0.1||cam1" {
		t.Errorf("reports = %q, want exactly one for the bad token on cam1, without the token", reports)
	}
}
