package linux

import (
	"net"
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// Unit tests for the /proc/net/tcp parser
// ---------------------------------------------------------------------------

// sampleTCP is a representative excerpt from /proc/net/tcp.
// Columns:  sl  local_address  rem_address  st  ...  inode
// Entry 0: 127.0.0.1:8080 listening (state 0A = LISTEN, remote 0.0.0.0:0)
// Entry 1: 192.168.1.10:44321 → 185.199.108.153:443 ESTABLISHED
// Entry 2: 10.0.0.1:22 listening
const sampleTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0A01A8C0:AD21 996CC7B9:01BB 01 00000000:00000000 00:00000000 00000000  1000        0 22222 1 0000000000000000 20 4 24 10 -1
   2: 0100000A:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 33333 1 0000000000000000 100 0 0 10 0
`

func TestParseTCPFile(t *testing.T) {
	// Write the sample to a temp file so readProcNetFile can open it.
	f, err := os.CreateTemp("", "proc_net_tcp_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(sampleTCP); err != nil {
		t.Fatal(err)
	}
	f.Close()

	conns, err := readProcNetFile(f.Name(), "tcp", parseIPv4Addr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(conns) != 3 {
		t.Fatalf("expected 3 connections, got %d", len(conns))
	}

	// Entry 0: 127.0.0.1:8080 LISTEN
	c0 := conns[0]
	if !c0.LocalIP.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("c0 local IP: got %s, want 127.0.0.1", c0.LocalIP)
	}
	if c0.LocalPort != 8080 {
		t.Errorf("c0 local port: got %d, want 8080", c0.LocalPort)
	}
	if c0.State != TCPListen {
		t.Errorf("c0 state: got %s, want LISTEN", c0.State)
	}
	if c0.IsOutbound() {
		t.Error("c0 should not be outbound (listener)")
	}

	// Entry 1: 192.168.1.10:44321 → 185.199.108.153:443 ESTABLISHED
	c1 := conns[1]
	if !c1.LocalIP.Equal(net.ParseIP("192.168.1.10")) {
		t.Errorf("c1 local IP: got %s, want 192.168.1.10", c1.LocalIP)
	}
	if c1.LocalPort != 44321 {
		t.Errorf("c1 local port: got %d, want 44321", c1.LocalPort)
	}
	if !c1.RemoteIP.Equal(net.ParseIP("185.199.108.153")) {
		t.Errorf("c1 remote IP: got %s, want 185.199.108.153", c1.RemoteIP)
	}
	if c1.RemotePort != 443 {
		t.Errorf("c1 remote port: got %d, want 443", c1.RemotePort)
	}
	if c1.State != TCPEstablished {
		t.Errorf("c1 state: got %s, want ESTABLISHED", c1.State)
	}
	if !c1.IsOutbound() {
		t.Error("c1 should be outbound")
	}
	if !c1.IsEstablished() {
		t.Error("c1 should be established")
	}
}

func TestParseIPv4(t *testing.T) {
	cases := []struct {
		hex  string
		want string
	}{
		{"0100007F", "127.0.0.1"},
		{"0101A8C0", "192.168.1.1"},
		{"00000000", "0.0.0.0"},
		{"0A01A8C0", "192.168.1.10"},
	}
	for _, tc := range cases {
		ip := parseIPv4Addr([]byte(tc.hex))
		if ip == nil {
			t.Errorf("parseIPv4Addr(%s) = nil", tc.hex)
			continue
		}
		if ip.String() != tc.want {
			t.Errorf("parseIPv4Addr(%s) = %s, want %s", tc.hex, ip.String(), tc.want)
		}
	}
}

func TestNetConnKey(t *testing.T) {
	c := NetConn{
		LocalIP:    net.ParseIP("192.168.1.10"),
		LocalPort:  44321,
		RemoteIP:   net.ParseIP("8.8.8.8"),
		RemotePort: 443,
	}
	key := c.Key()
	want := "192.168.1.10:44321->8.8.8.8:443"
	if key != want {
		t.Errorf("Key() = %q, want %q", key, want)
	}
}

// ---------------------------------------------------------------------------
// Benchmark: verify the fast parser is actually fast
// ---------------------------------------------------------------------------

// BenchmarkReadTCPFile measures parsing throughput on a simulated 200-conn file.
func BenchmarkReadTCPFile(b *testing.B) {
	// Build a realistic 200-entry /proc/net/tcp.
	var buf []byte
	buf = append(buf, []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")...)
	for i := 0; i < 200; i++ {
		buf = append(buf, []byte("   0: 0A01A8C0:AD21 996CC7B9:01BB 01 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 20 4 24 10 -1\n")...)
	}

	f, _ := os.CreateTemp("", "bench_procnet_*")
	f.Write(buf)
	f.Close()
	defer os.Remove(f.Name())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conns, _ := readProcNetFile(f.Name(), "tcp", parseIPv4Addr)
		_ = conns
	}
}
