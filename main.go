// nbdproxy keeps a local NBD session alive across backend transport failures.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

type config struct {
	Listen, Upstream, Export                           string
	Retry, ConnectTimeout, IOTimeout, HandshakeTimeout time.Duration
}

func endpoint(s string) (string, string, error) {
	if strings.HasPrefix(s, "unix:") && len(s) > 5 {
		addr := strings.TrimPrefix(s, "unix:")
		if !filepath.IsAbs(addr) || strings.ContainsRune(addr, 0) {
			return "", "", errors.New("unix socket path must be absolute and contain no NUL")
		}
		return "unix", addr, nil
	}
	if strings.HasPrefix(s, "tcp:") {
		addr := strings.TrimPrefix(s, "tcp:")
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", "", err
		}
		return "tcp", addr, nil
	}
	return "", "", errors.New("address must be tcp:host:port or unix:/absolute/path")
}
func (cfg config) check() error {
	if _, _, err := endpoint(cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if _, _, err := endpoint(cfg.Upstream); err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if cfg.Listen == cfg.Upstream {
		return errors.New("listen and upstream must differ")
	}
	if len(cfg.Export) > 4096 || strings.ContainsRune(cfg.Export, 0) {
		return errors.New("invalid export name")
	}
	if cfg.Retry <= 0 || cfg.ConnectTimeout <= 0 || cfg.IOTimeout <= 0 || cfg.HandshakeTimeout <= 0 {
		return errors.New("timeouts and retry interval must be positive")
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// connect retains no unbounded request queue or retry history.
func connect(ctx context.Context, cfg config, expected *export) (net.Conn, export, func(), error) {
	network, addr, _ := endpoint(cfg.Upstream)
	for {
		c, err := (&net.Dialer{Timeout: cfg.ConnectTimeout}).DialContext(ctx, network, addr)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { c.Close() })
			e, handshakeErr := upstreamHandshake(c, cfg.Export, cfg.HandshakeTimeout)
			if handshakeErr == nil {
				if expected != nil && e != *expected {
					stop()
					c.Close()
					return nil, e, func() {}, fmt.Errorf("backend geometry/flags changed: was %+v, now %+v; refusing replay", *expected, e)
				}
				return c, e, func() { stop(); c.Close() }, nil
			}
			stop()
			c.Close()
			err = handshakeErr
		}
		if ctx.Err() != nil {
			return nil, export{}, func() {}, ctx.Err()
		}
		slog.Warn("backend unavailable; holding client I/O", "error", err, "retry", cfg.Retry)
		if err = pause(ctx, cfg.Retry); err != nil {
			return nil, export{}, func() {}, err
		}
	}
}

type pending struct {
	r request
	p []byte
}

func readRequests(ctx context.Context, c net.Conn, out chan<- pending, cancel context.CancelFunc) {
	defer cancel()
	defer close(out)
	for {
		var r request
		if err := binary.Read(c, be, &r); err != nil {
			return
		}
		if r.Magic != requestMagic {
			return
		}
		if r.Type == 2 {
			return
		}
		// A WRITE payload must be consumed even for a rejected range or flag.
		// Oversized writes close the session rather than allocate attacker-controlled memory.
		if r.Type == 1 && r.Length > maxRequest {
			return
		}
		var p []byte
		if r.Type == 1 {
			p = make([]byte, r.Length)
			if _, err := io.ReadFull(c, p); err != nil {
				return
			}
		}
		select {
		case out <- pending{r, p}:
		case <-ctx.Done():
			return
		}
	}
}
func session(parent context.Context, client net.Conn, cfg config) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stopClient := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClient()
	defer client.Close()
	upstream, identity, closeUpstream, err := connect(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer func() { closeUpstream() }()
	public := identity.public()
	if err = downstreamHandshake(client, cfg.Export, public, cfg.HandshakeTimeout); err != nil {
		return err
	}
	slog.Info("client attached", "export", cfg.Export, "size", identity.Size)
	requests := make(chan pending)
	var reader sync.WaitGroup
	reader.Add(1)
	go func() { defer reader.Done(); readRequests(ctx, client, requests, cancel) }()
	defer func() { cancel(); client.Close(); reader.Wait() }()
	for {
		var item pending
		select {
		case <-ctx.Done():
			return ctx.Err()
		case q, ok := <-requests:
			if !ok {
				return nil
			}
			item = q
		}
		r := item.r
		if errno := validate(r, public); errno != 0 {
			client.SetWriteDeadline(time.Now().Add(cfg.IOTimeout))
			err = send(client, replyMagic, errno, r.Handle)
			client.SetWriteDeadline(time.Time{})
			if err != nil {
				return err
			}
			continue
		}
		var reply response
		var data []byte
		for {
			reply, data, err = exchange(upstream, r, item.p, cfg.IOTimeout)
			if err == nil {
				break
			}
			closeUpstream()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("backend connection lost; replaying pending request after reconnect", "command", r.Type, "offset", r.Offset, "error", err)
			if err = pause(ctx, cfg.Retry); err != nil {
				return err
			}
			upstream, _, closeUpstream, err = connect(ctx, cfg, &identity)
			if err != nil {
				return err
			}
			slog.Info("backend reconnected")
		}
		client.SetWriteDeadline(time.Now().Add(cfg.IOTimeout))
		err = send(client, reply, data)
		client.SetWriteDeadline(time.Time{})
		if err != nil {
			return err
		}
	}
}
func run(ctx context.Context, cfg config) error {
	if err := cfg.check(); err != nil {
		return err
	}
	network, addr, _ := endpoint(cfg.Listen)
	listener, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	if network == "unix" {
		if err = os.Chmod(addr, 0600); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	slog.Info("listening", "address", cfg.Listen, "upstream", cfg.Upstream, "export", cfg.Export, "version", version)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); wg.Wait() }()
	occupied := make(chan struct{}, 1)
	for {
		c, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case occupied <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-occupied }()
				if err := session(ctx, c, cfg); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
					slog.Warn("client session ended", "error", err)
				}
			}()
		default:
			slog.Warn("rejecting second client: one connection per proxy is supported")
			c.Close()
		}
	}
}
func main() {
	var cfg config
	var showVersion bool
	flag.StringVar(&cfg.Listen, "listen", "unix:/run/nbdproxy/nbd.sock", "local tcp:host:port or unix:/path")
	flag.StringVar(&cfg.Upstream, "upstream", "tcp:127.0.0.1:10809", "backend tcp:host:port or unix:/path")
	flag.StringVar(&cfg.Export, "export", "", "export name, unchanged across reconnects")
	flag.DurationVar(&cfg.Retry, "retry", time.Second, "delay between reconnect attempts")
	flag.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 5*time.Second, "backend dial timeout")
	flag.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 30*time.Second, "negotiation timeout")
	flag.DurationVar(&cfg.IOTimeout, "io-timeout", 60*time.Second, "backend request and client response timeout")
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.Parse()
	if showVersion {
		fmt.Println("nbdproxy", version)
		return
	}
	if flag.NArg() != 0 {
		slog.Error("unexpected positional arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		slog.Error("proxy failed", "error", err)
		os.Exit(1)
	}
}
