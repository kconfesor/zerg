package runner

import (
	"net"
	"testing"
)

// Two previews starting together are handed different ports.
//
// The operating system is asked for a free port and the listener closed at
// once, so between that and the agent binding it the number is free for the
// asking. Several cards landing at the same time is exactly when two runners
// start together, and the second server would have failed to bind for a reason
// nothing here would have explained.
func TestTwoRunnersAreNeverHandedTheSamePort(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}, reserved: map[int]bool{}}

	seen := map[int]bool{}
	for range 8 {
		ports, err := m.reservePorts(3)
		if err != nil {
			t.Fatalf("reservePorts: %v", err)
		}
		if len(ports) != 3 {
			t.Fatalf("got %d ports, want the whole block", len(ports))
		}
		for _, p := range ports {
			if seen[p] {
				t.Fatalf("port %d handed out twice", p)
			}
			seen[p] = true

			// And it is a port something can actually bind, which is the other
			// half of the promise: held open too long, this would hand back
			// numbers already in use.
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(p)))
			if err != nil {
				t.Fatalf("port %d was handed out but is not free: %v", p, err)
			}
			ln.Close()
		}
	}
}

// A stopped session's ports come back, or a daemon left running for a week
// leaks the range it was given.
func TestStoppingASessionGivesItsPortsBack(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}, reserved: map[int]bool{}}

	ports, err := m.reservePorts(3)
	if err != nil {
		t.Fatalf("reservePorts: %v", err)
	}
	m.mu.Lock()
	m.release(ports)
	held := len(m.reserved)
	m.mu.Unlock()

	if held != 0 {
		t.Errorf("%d ports still reserved after the session ended", held)
	}
}

func itoa(n int) string {
	return joinPorts([]int{n})
}
