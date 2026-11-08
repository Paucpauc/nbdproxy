//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Opt-in: needs root, a disposable /dev/nbd0, nbd-client and the nbd module.
// CI runs this against the distribution's nbd-client (no persist required).
func TestLinuxKernelRestart(t *testing.T) {
	if os.Getenv("NBDPROXY_KERNEL_TEST") != "1" {
		t.Skip("set NBDPROXY_KERNEL_TEST=1 on a disposable Linux host")
	}
	var mu sync.Mutex
	disk := make([]byte, 8<<20)
	startBackend := func(addr string) (string, func()) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		var conns sync.Map
		var wg sync.WaitGroup
		acceptDone := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(acceptDone)
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				conns.Store(c, true)
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer conns.Delete(c)
					defer c.Close()
					if backendHello(c, export{uint64(len(disk)), 1 | 4 | 8 | 32}) != nil {
						return
					}
					for {
						var r request
						if binary.Read(c, be, &r) != nil {
							return
						}
						var p []byte
						if r.Type == 1 {
							p = make([]byte, r.Length)
							if _, err := io.ReadFull(c, p); err != nil {
								return
							}
						}
						mu.Lock()
						var errno uint32
						if r.Offset > uint64(len(disk)) || uint64(r.Length) > uint64(len(disk))-r.Offset {
							errno = 22
						} else {
							switch r.Type {
							case 0:
								p = append([]byte(nil), disk[r.Offset:r.Offset+uint64(r.Length)]...)
							case 1:
								copy(disk[r.Offset:], p)
							case 3:
							case 4:
								clear(disk[r.Offset : r.Offset+uint64(r.Length)])
							default:
								errno = 22
							}
						}
						mu.Unlock()
						if send(c, replyMagic, errno, r.Handle) != nil {
							return
						}
						if r.Type == 0 && errno == 0 {
							if send(c, p) != nil {
								return
							}
						}
					}
				}()
			}
		}()
		return l.Addr().String(), func() {
			l.Close()
			<-acceptDone
			conns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
			wg.Wait()
		}
	}
	addr, stopBackend := startBackend("127.0.0.1:0")
	defer func() { stopBackend() }()
	dir := t.TempDir()
	sock := filepath.Join(dir, "proxy.sock")
	cfg := testConfig(addr)
	cfg.Listen = "unix:" + sock
	cfg.Retry = 20 * time.Millisecond
	cfg.IOTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy not listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	command := func(name string, args ...string) {
		t.Helper()
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	command("nbd-client", "-unix", sock, "/dev/nbd0", "-N", "disk", "-b", "4096")
	defer func() {
		out, err := exec.Command("nbd-client", "-d", "/dev/nbd0").CombinedOutput()
		if err != nil {
			t.Errorf("detach: %v: %s", err, out)
		}
	}()
	payload := bytes.Repeat([]byte{0x72}, 4096)
	in := filepath.Join(dir, "in")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(in, payload, 0600); err != nil {
		t.Fatal(err)
	}
	command("dd", "if="+in, "of=/dev/nbd0", "bs=4096", "count=1", "oflag=direct", "conv=fsync")
	stopBackend()
	// Reads issued while the backend is down must stay pending, then complete.
	readDone := make(chan error, 1)
	go func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		p, err := exec.CommandContext(cctx, "dd", "if=/dev/nbd0", "of="+out, "bs=4096", "count=1", "iflag=direct").CombinedOutput()
		if err != nil {
			t.Logf("dd: %s", p)
		}
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("read completed during outage: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	_, stopBackend = startBackend(addr)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("kernel read corrupted after restart")
	}
	// Verify writes and flushes continue after the reconnect, too.
	command("dd", "if="+in, "of=/dev/nbd0", "bs=4096", "seek=1", "count=1", "oflag=direct", "conv=fsync")
}
