//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestParseInitVersion(t *testing.T) {
	tests := []struct {
		output  string
		version string
		commit  string
		invalid bool
	}{
		{
			output:  "tini version 0.13.0 - git.949e6fa",
			version: "0.13.0",
			commit:  "949e6fa",
		}, {
			output:  "tini version 0.13.0\n",
			version: "0.13.0",
		}, {
			output:  "tini version 0.13.2",
			version: "0.13.2",
		}, {
			output:  "tini version 0.13.2 - ",
			version: "0.13.2",
		}, {
			output: " - git.949e6fa",
			commit: "949e6fa",
		}, {
			output:  "tini version0.13.2",
			invalid: true,
		}, {
			output:  "version 0.13.0",
			invalid: true,
		}, {
			output:  "",
			invalid: true,
		}, {
			output:  " - ",
			invalid: true,
		}, {
			output:  "hello world",
			invalid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.output, func(t *testing.T) {
			version, commit, err := parseInitVersion(tc.output)
			if tc.invalid {
				assert.Check(t, is.ErrorContains(err, ""))
			} else {
				assert.Check(t, err)
			}
			assert.Equal(t, tc.version, version)
			assert.Equal(t, tc.commit, commit)
		})
	}
}

func TestParseRuntimeVersion(t *testing.T) {
	tests := []struct {
		output  string
		runtime string
		version string
		commit  string
		invalid bool
	}{
		{
			output: `
runc version 1.0.0-rc5+dev
commit: 69663f0bd4b60df09991c08812a60108003fa340
spec: 1.0.0
`,
			runtime: "runc",
			version: "1.0.0-rc5+dev",
			commit:  "69663f0bd4b60df09991c08812a60108003fa340",
		},
		{
			output: `
runc version 1.0.0-rc5+dev
spec: 1.0.0
`,
			runtime: "runc",
			version: "1.0.0-rc5+dev",
		},
		{
			output: `
commit: 69663f0bd4b60df09991c08812a60108003fa340
spec: 1.0.0
`,
			commit: "69663f0bd4b60df09991c08812a60108003fa340",
		},
		{
			output: `
crun version 0.7
spec: 1.0.0
+SYSTEMD +SELINUX +CAP +SECCOMP +EBPF +YAJL
`,
			runtime: "crun",
			version: "0.7",
		},
		{
			output:  "",
			invalid: true,
		},
		{
			output:  "hello world",
			invalid: true,
		},
	}

	for _, tc := range tests {
		runtime, version, commit, err := parseRuntimeVersion(tc.output)
		if tc.invalid {
			assert.Check(t, is.ErrorContains(err, ""))
		} else {
			assert.Check(t, err)
		}
		assert.Equal(t, tc.runtime, runtime)
		assert.Equal(t, tc.version, version)
		assert.Equal(t, tc.commit, commit)
	}
}

// countingListener wraps a net.Listener and tracks how many accepted
// connections are still open.
type countingListener struct {
	net.Listener
	open *atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	return &countingConn{Conn: c, onClose: func() { l.open.Add(-1) }}, nil
}

type countingConn struct {
	net.Conn
	closed  atomic.Bool
	onClose func()
}

func (c *countingConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.onClose()
	}
	return c.Conn.Close()
}

// TestGetRootlessKitInfoClosesConnections is a regression test for
// https://github.com/moby/moby/issues/53814: every version lookup created a
// new RootlessKit client (and with it a fresh http.Transport) whose idle
// connection to the RootlessKit API socket was never released, leaking one
// connection per call on both dockerd and rootlesskit.
func TestGetRootlessKitInfoClosesConnections(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")

	var openConns atomic.Int32
	ln, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"apiVersion":"1.1.3","version":"3.2.0","stateDir":"/tmp/fake"}`)
		}),
	}
	go func() { _ = srv.Serve(&countingListener{Listener: ln, open: &openConns}) }()
	defer func() { _ = srv.Close() }()

	t.Setenv("ROOTLESSKIT_STATE_DIR", filepath.Dir(sock))

	info, err := getRootlessKitInfo(context.Background())
	assert.NilError(t, err)
	assert.Equal(t, "3.2.0", info.Version)

	// The server only notices the close once it happens; poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for openConns.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, int32(0), openConns.Load(), "connection to the RootlessKit API socket was not released")
}
