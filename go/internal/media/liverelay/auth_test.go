package liverelay

import (
	"net"
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

// A reader with wrong credentials is reported (for the security log); the
// unauthenticated first DESCRIBE of the RTSP challenge is not, and neither is
// a reader with the right pair.
func TestRelayReportsCredentialedAuthFailures(t *testing.T) {
	addr := freeAddr(t)
	var mu sync.Mutex
	var reports []string
	r := &Relay{
		Address: addr,
		CredsFn: func() [][2]string { return [][2]string{{"good", "pass"}} },
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

	describe := func(userinfo string) {
		u, err := base.ParseURL("rtsp://" + userinfo + addr + "/cam1")
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
	describe("good:pass@")
	describe("mallory:guess@")

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || reports[0] != "127.0.0.1|mallory|cam1" {
		t.Errorf("reports = %q, want exactly one for mallory from 127.0.0.1 on cam1", reports)
	}
}
