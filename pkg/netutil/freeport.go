package netutil

import (
	"fmt"
	"net"
)

// FindFreePort returns an available TCP port on localhost.
func FindFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// FindFreePortFrom checks whether port is available and returns it if so.
// Otherwise it increments until it finds a free port, trying up to 100 ports.
// A port is considered free only if both 127.0.0.1 and 0.0.0.0 can bind it.
func FindFreePortFrom(port int) (int, error) {
	for i := 0; i < 100; i++ {
		candidate := port + i
		if portFree(candidate) {
			return candidate, nil
		}
	}
	return 0, fmt.Errorf("no free port found in range %d–%d", port, port+99)
}

// portFree returns true if the given port is not bound on either loopback or
// all interfaces.
func portFree(port int) bool {
	addr := fmt.Sprintf(":%d", port)
	for _, host := range []string{"127.0.0.1", ""} {
		if host != "" {
			addr = fmt.Sprintf("%s:%d", host, port)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}
