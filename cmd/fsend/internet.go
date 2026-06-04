package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/signaling"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// signalingClient builds a client pointed at the configured server.
//
// HTTPS is the default for non-local addresses; HTTP for localhost so dev
// loops work without a cert.
func signalingClient(cfg *config.Config) (*signaling.Client, string) {
	addr := cfg.EffectiveServer()
	baseURL := addr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		if isLocalAddr(addr) {
			baseURL = "http://" + addr
		} else {
			baseURL = "https://" + addr
		}
	}
	return signaling.New(baseURL, version.Version), addr
}

func isLocalAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// runSendOverRelay performs the cross-internet send flow when LAN
// discovery has failed (or been disabled).
func runSendOverRelay(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, cfg *config.Config) error {
	client, _ := signalingClient(cfg)

	created, err := client.Create(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Delete(context.Background(), created.SessionID) }()

	printSendArtifact(created.Code, items, kind)

	// Long-poll until the receiver pairs.
	deadline := time.Now().Add(time.Duration(created.TTLSeconds) * time.Second)
	var paired bool
	for time.Now().Before(deadline) && !paired {
		resp, err := client.Wait(ctx, created.Code)
		if err != nil {
			return err
		}
		if resp != nil {
			paired = true
		}
	}
	if !paired {
		return fmt.Errorf("%w: receiver did not arrive within %ds", fserrors.ErrPromptTimeout, created.TTLSeconds)
	}

	alloc, err := client.AllocateRelay(ctx, created.SessionID)
	if err != nil {
		return fmt.Errorf("allocating relay: %w", err)
	}

	myUDP, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return fmt.Errorf("local UDP: %w", err)
	}
	defer myUDP.Close()

	relayUDPAddr, err := net.ResolveUDPAddr("udp", alloc.RelayAddr)
	if err != nil {
		return fmt.Errorf("resolving relay: %w", err)
	}
	tok, err := relay.ParseToken(alloc.SessionToken)
	if err != nil {
		return fmt.Errorf("parsing session token: %w", err)
	}
	relayConn := relay.NewClient(myUDP, relayUDPAddr, tok, "peer-receiver")
	if _, err := relayConn.WriteTo([]byte{0}, nil); err != nil {
		return fmt.Errorf("relay bootstrap: %w", err)
	}

	tlsCfg, err := quicconn.SenderTLSConfig()
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: relayConn}
	defer tr.Close()
	ln, err := tr.Listen(tlsCfg, quicconn.QuicConfig())
	if err != nil {
		return fmt.Errorf("QUIC listen: %w", err)
	}
	defer ln.Close()

	fmt.Fprintln(os.Stderr, marker("⚠", "[WARN]"), "Relayed via", alloc.RelayAddr)

	qc, err := ln.Accept(ctx)
	if err != nil {
		return fmt.Errorf("QUIC accept: %w", err)
	}
	res, err := quicconn.SenderHandshake(ctx, qc)
	if err != nil {
		return err
	}
	defer res.Close()

	if err := transfer.Send(ctx, &res.Streams, transfer.SendOptions{
		Items:         items,
		Hostname:      hostnameOrDefault(f.hostname),
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TransferKind:  kind,
		Compress:      !f.noCompress,
	}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	return nil
}

// runReceiveOverRelay performs the cross-internet receive flow.
func runReceiveOverRelay(ctx context.Context, f *flags, c string, cfg *config.Config) error {
	client, _ := signalingClient(cfg)

	joined, err := client.Join(ctx, c)
	if err != nil {
		return err
	}

	alloc, err := client.AllocateRelay(ctx, joined.SessionID)
	if err != nil {
		return fmt.Errorf("allocating relay: %w", err)
	}

	myUDP, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return fmt.Errorf("local UDP: %w", err)
	}
	defer myUDP.Close()

	relayUDPAddr, err := net.ResolveUDPAddr("udp", alloc.RelayAddr)
	if err != nil {
		return fmt.Errorf("resolving relay: %w", err)
	}
	tok, err := relay.ParseToken(alloc.SessionToken)
	if err != nil {
		return fmt.Errorf("parsing session token: %w", err)
	}
	relayConn := relay.NewClient(myUDP, relayUDPAddr, tok, "peer-sender")

	if _, err := relayConn.WriteTo([]byte{0}, nil); err != nil {
		return fmt.Errorf("relay bootstrap: %w", err)
	}

	tr := &quic.Transport{Conn: relayConn}
	defer tr.Close()

	dialAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dialCancel()
	qc, err := tr.Dial(dialCtx, dialAddr, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
	if err != nil {
		return fmt.Errorf("QUIC dial: %w", err)
	}
	res, err := quicconn.ReceiverHandshake(ctx, qc)
	if err != nil {
		return err
	}
	defer res.Close()

	fmt.Fprintln(os.Stderr, marker("⚠", "[WARN]"), "Relayed via", alloc.RelayAddr)

	outDir := f.outDir
	if outDir == "" {
		outDir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	if err := transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
		Hostname:      hostnameOrDefault(f.hostname),
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TargetDir:     outDir,
		Overwrite:     f.overwrite,
		Accept:        func(h wire.SenderHello) bool { return promptAccept(f, h) },
	}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	return nil
}

func hostnameOrDefault(s string) string {
	if s != "" {
		return s
	}
	h, _ := os.Hostname()
	return h
}
