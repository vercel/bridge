// Package socks implements a minimal SOCKS5 server (RFC 1928) that supports
// the CONNECT command. It is used by the bridge interceptor as an alternative
// to iptables-based transparent proxying in environments where kernel-level
// interception is not available (e.g. Vercel Sandbox / Firecracker VMs).
package socks

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
)

const (
	socks5Version = 0x05

	authNone         = 0x00
	authNoAcceptable = 0xFF

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess              = 0x00
	repGeneralFailure       = 0x01
	repCommandNotSupported  = 0x07
	repAddrTypeNotSupported = 0x08
)

// ConnHandler is called for each successful SOCKS5 CONNECT request.
// The conn is ready for bidirectional data transfer. The handler owns the
// connection and is responsible for closing it.
type ConnHandler func(conn net.Conn, dest string, hostname string)

// Server is a minimal SOCKS5 server that accepts CONNECT requests and
// delegates them to a ConnHandler.
type Server struct {
	listener net.Listener
	handler  ConnHandler
	wg       sync.WaitGroup
}

// New creates and starts listening on the given address. Call Start() to
// begin accepting connections.
func New(addr string, handler ConnHandler) (*Server, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("socks: listen %s: %w", addr, err)
	}
	return &Server{
		listener: lis,
		handler:  handler,
	}, nil
}

// Addr returns the listener's address.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Start begins accepting connections in the background.
func (s *Server) Start() {
	slog.Info("SOCKS5 proxy listening", "addr", s.listener.Addr().String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				return // listener closed
			}
			go s.handle(conn)
		}
	}()
}

// Stop closes the listener and waits for the accept loop to exit.
func (s *Server) Stop() {
	s.listener.Close()
	s.wg.Wait()
}

func (s *Server) handle(conn net.Conn) {
	dest, hostname, err := s.handshake(conn)
	if err != nil {
		slog.Debug("SOCKS5 handshake failed", "remote", conn.RemoteAddr(), "error", err)
		conn.Close()
		return
	}

	slog.Info("SOCKS5 CONNECT", "dest", dest, "hostname", hostname, "remote", conn.RemoteAddr())
	s.handler(conn, dest, hostname)
}

// handshake performs the SOCKS5 auth negotiation and CONNECT request.
// Returns the destination address ("host:port") and hostname (empty if IP).
func (s *Server) handshake(conn net.Conn) (string, string, error) {
	// --- Auth negotiation ---
	// Client: [VER, NMETHODS, METHODS...]
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", "", fmt.Errorf("read auth header: %w", err)
	}
	if header[0] != socks5Version {
		return "", "", fmt.Errorf("unsupported version: %d", header[0])
	}

	nMethods := int(header[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", "", fmt.Errorf("read auth methods: %w", err)
	}

	// We only support no-auth.
	hasNoAuth := false
	for _, m := range methods {
		if m == authNone {
			hasNoAuth = true
			break
		}
	}
	if !hasNoAuth {
		conn.Write([]byte{socks5Version, authNoAcceptable})
		return "", "", fmt.Errorf("no acceptable auth method")
	}

	// Server: [VER, METHOD=0x00]
	if _, err := conn.Write([]byte{socks5Version, authNone}); err != nil {
		return "", "", fmt.Errorf("write auth reply: %w", err)
	}

	// --- CONNECT request ---
	// Client: [VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT]
	reqHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHeader); err != nil {
		return "", "", fmt.Errorf("read request header: %w", err)
	}
	if reqHeader[0] != socks5Version {
		return "", "", fmt.Errorf("request version mismatch: %d", reqHeader[0])
	}
	if reqHeader[1] != cmdConnect {
		s.sendReply(conn, repCommandNotSupported)
		return "", "", fmt.Errorf("unsupported command: %d", reqHeader[1])
	}

	atyp := reqHeader[3]
	var host string
	var hostname string

	switch atyp {
	case atypIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", "", fmt.Errorf("read ipv4 addr: %w", err)
		}
		host = net.IP(addr).String()

	case atypDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", "", fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", "", fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
		hostname = host

	case atypIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", "", fmt.Errorf("read ipv6 addr: %w", err)
		}
		host = net.IP(addr).String()

	default:
		s.sendReply(conn, repAddrTypeNotSupported)
		return "", "", fmt.Errorf("unsupported address type: %d", atyp)
	}

	// Read port (2 bytes, big-endian).
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", "", fmt.Errorf("read port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBuf)

	// Send success reply.
	// Server: [VER=5, REP=0, RSV=0, ATYP=1, BND.ADDR=0.0.0.0, BND.PORT=0]
	if err := s.sendReply(conn, repSuccess); err != nil {
		return "", "", fmt.Errorf("write reply: %w", err)
	}

	dest := net.JoinHostPort(host, strconv.Itoa(int(port)))
	return dest, hostname, nil
}

func (s *Server) sendReply(conn net.Conn, rep byte) error {
	// [VER, REP, RSV, ATYP=IPv4, BND.ADDR=0.0.0.0, BND.PORT=0]
	reply := []byte{socks5Version, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0}
	_, err := conn.Write(reply)
	return err
}
