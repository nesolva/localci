package exec

import (
	"fmt"
	"net"
	"sync"
)

var (
	portMu   sync.Mutex
	reserved = map[int]struct{}{}
)

// FreePorts returns n distinct loopback ports that are free now and that no
// other caller in this process holds. They stay reserved until ReleasePorts.
func FreePorts(n int) ([]int, error) {
	portMu.Lock()
	defer portMu.Unlock()
	var (
		ports     []int
		listeners []net.Listener
	)
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	// The listeners stay open until all n are found, so none repeats.
	for tries := 0; len(ports) < n; tries++ {
		if tries > 20*n+100 {
			return nil, fmt.Errorf("no free port found after %d tries", tries)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		p := l.Addr().(*net.TCPAddr).Port
		if _, taken := reserved[p]; taken {
			l.Close()
			continue
		}
		listeners = append(listeners, l)
		ports = append(ports, p)
	}
	for _, p := range ports {
		reserved[p] = struct{}{}
	}
	return ports, nil
}

// ReleasePorts gives ports back once nothing uses them.
func ReleasePorts(ports ...int) {
	portMu.Lock()
	defer portMu.Unlock()
	for _, p := range ports {
		delete(reserved, p)
	}
}
