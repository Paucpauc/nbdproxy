package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func testConfig(addr string) config {
	return config{Listen: "tcp:127.0.0.1:0", Upstream: "tcp:" + addr, Export: "disk", Retry: time.Millisecond, ConnectTimeout: time.Second, HandshakeTimeout: time.Second, IOTimeout: time.Second}
}
func pair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}
func backendHello(c net.Conn, e export) error {
	if err := send(c, nbdMagic, optMagic, uint16(3)); err != nil {
		return err
	}
	var f uint32
	if err := binary.Read(c, be, &f); err != nil {
		return err
	}
	var h struct {
		Magic       uint64
		Opt, Length uint32
	}
	if err := binary.Read(c, be, &h); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, c, int64(h.Length)); err != nil {
		return err
	}
	return send(c, e)
}
func roundtrip(t *testing.T, c net.Conn, r request, p []byte) []byte {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := send(c, r, p); err != nil {
		t.Fatal(err)
	}
	var reply response
	if err := binary.Read(c, be, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Magic != replyMagic || reply.Handle != r.Handle || reply.Error != 0 {
		t.Fatalf("reply %+v", reply)
	}
	var data []byte
	if r.Type == 0 {
		data = make([]byte, r.Length)
		if _, err := io.ReadFull(c, data); err != nil {
			t.Fatal(err)
		}
	}
	return data
}
func TestReplayLostWriteReplyAndPartialRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	e := export{4096, 1 | 4 | 8 | 32 | 256}
	backendResult := make(chan error, 1)
	go func() {
		var stored []byte
		for attempt := 0; attempt < 3; attempt++ {
			c, err := listener.Accept()
			if err != nil {
				backendResult <- err
				return
			}
			if err = backendHello(c, e); err != nil {
				c.Close()
				backendResult <- err
				return
			}
			for {
				var r request
				if err = binary.Read(c, be, &r); err != nil {
					break
				}
				var p []byte
				if r.Type == 1 {
					p = make([]byte, r.Length)
					_, err = io.ReadFull(c, p)
					if err != nil {
						break
					}
					stored = p
				}
				// First write is committed, but its entire reply is lost.
				if attempt == 0 {
					c.Close()
					break
				}
				if r.Type == 0 {
					if err = send(c, replyMagic, uint32(0), r.Handle); err != nil {
						break
					}
					// On the next connection lose a read halfway through its payload.
					if attempt == 1 {
						send(c, stored[:len(stored)/2])
						c.Close()
						break
					}
					err = send(c, stored)
					c.Close()
					backendResult <- err
					return
				}
				if err = send(c, replyMagic, uint32(0), r.Handle); err != nil {
					break
				}
			}
			c.Close()
		}
	}()
	client, server := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session(ctx, server, testConfig(listener.Addr().String())) }()
	got, err := upstreamHandshake(client, "disk", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Flags&256 != 0 {
		t.Fatal("multi-conn advertised")
	}
	payload := bytes.Repeat([]byte{0x5a}, 4096)
	roundtrip(t, client, request{requestMagic, 1, 1, 11, 0, 4096}, payload)
	roundtrip(t, client, request{requestMagic, 0, 3, 12, 0, 0}, nil)
	actual := roundtrip(t, client, request{requestMagic, 0, 0, 13, 0, 4096}, nil)
	if !bytes.Equal(actual, payload) {
		t.Fatal("corrupt replayed read")
	}
	if err := <-backendResult; err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session leaked")
	}
}
func TestChangedBackendRefusesReplay(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for _, size := range []uint64{4096, 8192} {
			c, err := l.Accept()
			if err != nil {
				return
			}
			backendHello(c, export{size, 1})
			var r request
			binary.Read(c, be, &r)
			c.Close()
		}
	}()
	client, server := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session(ctx, server, testConfig(l.Addr().String())) }()
	if _, err := upstreamHandshake(client, "disk", time.Second); err != nil {
		t.Fatal(err)
	}
	send(client, request{requestMagic, 0, 0, 1, 0, 512})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected identity error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not reject changed backend")
	}
}
func TestCancelUnavailableBackend(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(addr)
	cfg.Retry = time.Hour
	done := make(chan error, 1)
	go func() { _, _, _, err := connect(ctx, cfg, nil); done <- err }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
}
func TestValidation(t *testing.T) {
	e := export{4096, 1 | 4 | 8 | 32}
	cases := []struct {
		name string
		r    request
		want uint32
	}{
		{"overflow", request{Offset: ^uint64(0), Length: 512}, 22},
		{"end", request{Offset: 4096, Length: 1}, 22},
		{"unsupported command", request{Type: 6}, 22},
		{"read FUA", request{Flags: 1, Length: 512}, 22},
		{"flush range", request{Type: 3, Length: 1}, 22},
		{"valid write", request{Flags: 1, Type: 1, Length: 512}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validate(tc.r, e); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	if validate(request{Type: 1, Length: 512}, export{4096, 3}) != 1 {
		t.Fatal("read-only write accepted")
	}
}
func TestGOAndMalformedOptions(t *testing.T) {
	client, server := pair(t)
	e := export{4096, 1 | 4}
	done := make(chan error, 1)
	go func() { done <- downstreamHandshake(server, "disk", e, time.Second) }()
	var hello [18]byte
	if _, err := io.ReadFull(client, hello[:]); err != nil {
		t.Fatal(err)
	}
	send(client, uint32(3))
	// Bad GO gets an error without destroying negotiation.
	send(client, optMagic, uint32(7), uint32(1), []byte{0})
	var h struct {
		Magic             uint64
		Opt, Kind, Length uint32
	}
	if err := binary.Read(client, be, &h); err != nil {
		t.Fatal(err)
	}
	if h.Kind != 0x80000003 {
		t.Fatalf("bad option reply %+v", h)
	}
	p := make([]byte, 10)
	be.PutUint32(p, 4)
	copy(p[4:], "disk")
	send(client, optMagic, uint32(7), uint32(len(p)), p)
	infos := 0
	for {
		if err := binary.Read(client, be, &h); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, h.Length)
		io.ReadFull(client, payload)
		if h.Kind == 1 {
			break
		}
		if h.Kind != 3 {
			t.Fatalf("unexpected %+v", h)
		}
		infos++
	}
	if infos != 2 {
		t.Fatalf("expected export + block size info, got %d", infos)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestReadAheadBoundAndDisconnect(t *testing.T) {
	client, server := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan pending)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); readRequests(ctx, server, out, cancel) }()
	go func() {
		send(client, request{requestMagic, 0, 1, 1, 0, 1}, []byte{1})
		send(client, request{requestMagic, 0, 1, 2, 0, 1}, []byte{2})
	}()
	q := <-out
	if len(q.p) != 1 || q.p[0] != 1 {
		t.Fatal("wrong first request")
	}
	q = <-out
	if q.p[0] != 2 {
		t.Fatal("wrong second request")
	}
	client.Close()
	wg.Wait()
	if ctx.Err() == nil {
		t.Fatal("disconnect did not cancel session")
	}
}
func FuzzExportName(f *testing.F) {
	f.Add(uint32(7), []byte{0, 0, 0, 0, 0, 0})
	f.Add(uint32(1), []byte("disk"))
	f.Fuzz(func(t *testing.T, opt uint32, p []byte) { exportName(opt, p) })
}

func TestUnixListenerSingleClientAndShutdown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nbdp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", dir+"/backend.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if backendHello(c, export{4096, 1}) != nil {
			return
		}
		io.Copy(io.Discard, c)
	}()
	cfg := testConfig("unused:0")
	cfg.Upstream = "unix:" + l.Addr().String()
	path := dir + "/front.sock"
	cfg.Listen = "unix:" + path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	var client net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	defer client.Close()
	if _, err := upstreamHandshake(client, "disk", time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(time.Second))
	var p [1]byte
	if _, err = second.Read(p[:]); err != io.EOF {
		t.Fatalf("second client should be closed, got %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown leaked session")
	}
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("backend socket left open")
	}
}

func TestBackendOutageHoldsPendingRead(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	requestSeen := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	backendDone := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		if err = backendHello(c, export{4096, 1}); err != nil {
			c.Close()
			backendDone <- err
			return
		}
		var r request
		err = binary.Read(c, be, &r)
		c.Close()
		close(requestSeen)
		if err != nil {
			backendDone <- err
			return
		}
		<-resume
		// Reconnect can be in TCP backlog while negotiation remains pending.
		c, err = l.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer c.Close()
		if err = backendHello(c, export{4096, 1}); err != nil {
			backendDone <- err
			return
		}
		if err = binary.Read(c, be, &r); err != nil {
			backendDone <- err
			return
		}
		backendDone <- send(c, replyMagic, uint32(0), r.Handle, make([]byte, r.Length))
	}()
	client, server := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session(ctx, server, testConfig(l.Addr().String())) }()
	if _, err := upstreamHandshake(client, "disk", time.Second); err != nil {
		t.Fatal(err)
	}
	if err = send(client, request{requestMagic, 0, 0, 1, 0, 512}); err != nil {
		t.Fatal(err)
	}
	<-requestSeen
	client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var one [1]byte
	if _, err = client.Read(one[:]); err == nil {
		t.Fatal("unexpected early reply")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("local socket closed during outage: %v", err)
	}
	close(resume)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var reply response
	if err = binary.Read(client, be, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != 0 || reply.Handle != 1 {
		t.Fatalf("bad reply %+v", reply)
	}
	if _, err = io.CopyN(io.Discard, client, 512); err != nil {
		t.Fatal(err)
	}
	if err = <-backendDone; err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session did not stop")
	}
}
