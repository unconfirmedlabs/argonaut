package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestConfigParsesValidJSON(t *testing.T) {
	input := `{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443}]}`
	config, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(config, validationModeEnclave); err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(config.Endpoints))
	}
	if config.Endpoints[0].Host != "sui.io" {
		t.Fatalf("expected host sui.io, got %s", config.Endpoints[0].Host)
	}
	if config.Endpoints[0].VsockPort != 8443 {
		t.Fatalf("expected vsock port 8443, got %d", config.Endpoints[0].VsockPort)
	}
}

func TestConfigParsesEmptyEndpoints(t *testing.T) {
	input := `{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[]}`
	config, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(config, validationModeEnclave); err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 0 {
		t.Fatalf("expected 0 endpoints, got %d", len(config.Endpoints))
	}
}

func TestConfigRejectsMissingFields(t *testing.T) {
	input := `{}`
	config, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(config, validationModeEnclave); err == nil {
		t.Fatal("expected missing fields to fail validation")
	}
}

func TestLoopbackIPGeneration(t *testing.T) {
	for i := 0; i < 191; i++ {
		ip := endpointLocalIP(Endpoint{}, i)
		if net.ParseIP(ip) == nil {
			t.Fatalf("invalid IP: %s", ip)
		}
	}
}

func TestHostsFileContent(t *testing.T) {
	endpoints := []Endpoint{
		{Host: "sui.io", VsockPort: 8001},
		{Host: "walrus.io", VsockPort: 8002},
	}
	contentBytes, err := renderHosts(endpoints)
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)

	if !strings.Contains(content, "127.0.0.1   localhost") {
		t.Fatal("missing localhost entry")
	}
	if !strings.Contains(content, "127.0.0.64   sui.io") {
		t.Fatal("missing sui.io entry")
	}
	if !strings.Contains(content, "127.0.0.65   walrus.io") {
		t.Fatal("missing walrus.io entry")
	}
	if !strings.Contains(content, hostsBeginMarker) || !strings.Contains(content, hostsEndMarker) {
		t.Fatal("missing managed block markers")
	}
}

func TestEndpointLocalIPOverride(t *testing.T) {
	input := `{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443,"localIP":"127.0.0.1","localPort":9443}]}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeEnclave); err != nil {
		t.Fatal(err)
	}
	if got := endpointLocalIP(cfg.Endpoints[0], 0); got != "127.0.0.1" {
		t.Fatalf("localIP override: got %s", got)
	}
	content, err := renderHosts(cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "127.0.0.1   sui.io") {
		t.Fatalf("expected localIP override in hosts file, got:\n%s", content)
	}
}

func TestConfigRejectsInvalidJSON(t *testing.T) {
	input := `{not valid json}`
	if _, err := ParseConfig([]byte(input)); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestMaxEndpointLimit(t *testing.T) {
	endpoints := make([]string, 0, maxEndpoints)
	for i := 0; i < maxEndpoints; i++ {
		endpoints = append(endpoints, fmt.Sprintf(`{"host":"h%d.example","vsockPort":%d}`, i, 1000+i))
	}
	valid := fmt.Sprintf(`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[%s]}`, strings.Join(endpoints, ","))
	cfg, err := ParseConfig([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeEnclave); err != nil {
		t.Fatalf("expected %d endpoints to be valid: %v", maxEndpoints, err)
	}

	endpoints = append(endpoints, `{"host":"overflow.example","vsockPort":9000}`)
	invalid := fmt.Sprintf(`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[%s]}`, strings.Join(endpoints, ","))
	cfg, err = ParseConfig([]byte(invalid))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeEnclave); err == nil {
		t.Fatal("expected endpoint overflow to fail validation")
	}
}

func TestHostsFileNoInjection(t *testing.T) {
	maliciousHosts := []string{
		"evil.com localhost",
		"evil.com\n127.0.0.1 admin",
		"evil.com\tlocalhost",
		"evil.com/path",
		"evil.com:443",
	}
	for _, host := range maliciousHosts {
		t.Run(host, func(t *testing.T) {
			endpoints := []Endpoint{{Host: host, VsockPort: 8001}}
			if _, err := renderHosts(endpoints); err == nil {
				t.Fatalf("expected renderHosts to reject %q", host)
			}
		})
	}
}

func TestMergeHostsPreservesExistingContent(t *testing.T) {
	existing := []byte("127.0.0.1   localhost\n10.0.0.2   internal\n")
	content, err := mergeHosts(existing, []Endpoint{{Host: "sui.io", VsockPort: 8001}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	if !strings.Contains(got, "10.0.0.2   internal") {
		t.Fatal("expected existing host entry to be preserved")
	}
	if !strings.Contains(got, "127.0.0.64   sui.io") {
		t.Fatal("expected managed endpoint entry")
	}
}

func TestMergeHostsReplacesManagedBlock(t *testing.T) {
	existing := []byte("127.0.0.1   localhost\n\n# argonaut begin\n127.0.0.64   old.example\n# argonaut end\n")
	content, err := mergeHosts(existing, []Endpoint{{Host: "new.example", VsockPort: 8001}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	if strings.Contains(got, "old.example") {
		t.Fatal("expected old managed entry to be removed")
	}
	if !strings.Contains(got, "new.example") {
		t.Fatal("expected new managed entry")
	}
}

func TestConfigValidationRejectsBadEndpoints(t *testing.T) {
	cases := []string{
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"vsockPort":8443}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"","vsockPort":8443}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"evil.com/path","vsockPort":8443}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":0}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":70000}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443,"tcpPort":0}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443,"localIP":""}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443,"localIP":"10.0.0.1"}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443},{"host":"sui.io","vsockPort":8444}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443},{"host":"walrus.io","vsockPort":8443}]}`,
		`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443,"localIP":"127.0.0.1","localPort":9443},{"host":"walrus.io","vsockPort":8444,"localIP":"127.0.0.1","localPort":9443}]}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(input))
			if err != nil {
				return
			}
			if err := ValidateConfig(cfg, validationModeEnclave); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEndpointPortDefaults(t *testing.T) {
	input := `{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443}]}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeEnclave); err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].TCPPort != defaultTCPPort {
		t.Fatalf("tcpPort default: got %d", cfg.Endpoints[0].TCPPort)
	}
	if cfg.Endpoints[0].LocalPort != defaultTCPPort {
		t.Fatalf("localPort default: got %d", cfg.Endpoints[0].LocalPort)
	}
}

func TestConfigSizeLimit(t *testing.T) {
	_, err := readAllLimited(strings.NewReader(strings.Repeat("x", maxConfigBytes+1)), maxConfigBytes)
	if err == nil {
		t.Fatal("expected oversized input to fail")
	}
}

func TestLoopbackIPBoundary(t *testing.T) {
	max := fmt.Sprintf("127.0.0.%d", 64+maxEndpoints-1)
	if net.ParseIP(max) == nil {
		t.Fatalf("max endpoint IP should be valid: %s", max)
	}
	overflow := fmt.Sprintf("127.0.0.%d", 64+maxEndpoints)
	if overflow != "127.0.0.255" {
		t.Fatalf("expected first overflow IP to be 127.0.0.255, got %s", overflow)
	}
}

func TestHostConfigParsesValidJSON(t *testing.T) {
	input := `{"httpPort":8080,"httpVsockPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8104}]}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeHost); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPPort != 8080 {
		t.Fatalf("expected httpPort 8080, got %d", cfg.HTTPPort)
	}
	if cfg.HTTPVsockPort != 3000 {
		t.Fatalf("expected httpVsockPort 3000, got %d", cfg.HTTPVsockPort)
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0].Host != "sui.io" {
		t.Fatalf("unexpected endpoints: %+v", cfg.Endpoints)
	}
}

func TestHostConfigIgnoresExtraFields(t *testing.T) {
	input := `{"httpPort":8080,"httpVsockPort":3000,"endpoints":[],"secrets":{"key":"val"},"app":{"foo":1},"logLevel":"debug"}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeHost); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPPort != 8080 {
		t.Fatalf("expected httpPort 8080, got %d", cfg.HTTPPort)
	}
}

func TestHostConfigMultipleEndpoints(t *testing.T) {
	input := `{"httpPort":8080,"httpVsockPort":3000,"endpoints":[
		{"host":"fullnode.testnet.sui.io","vsockPort":8104},
		{"host":"seal.mirai.cloud","vsockPort":8101},
		{"host":"walrus.space","vsockPort":8103}
	]}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeHost); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 3 {
		t.Fatalf("expected 3 endpoints, got %d", len(cfg.Endpoints))
	}
}

func TestHostConfigZeroHTTPPortDetected(t *testing.T) {
	input := `{"httpVsockPort":3000,"endpoints":[]}`
	cfg, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(cfg, validationModeHost); err == nil {
		t.Fatal("expected missing httpPort to fail validation")
	}
}

// --- DNS resolution tests (ported from aws-nitro-enclaves-cli vsock_proxy/src/dns.rs) ---

func TestResolveValidDomain(t *testing.T) {
	addrs, err := net.LookupHost("localhost")
	if err != nil {
		t.Fatalf("failed to resolve localhost: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("expected at least one address for localhost")
	}
}

func TestResolveInvalidDomain(t *testing.T) {
	_, err := net.LookupHost("invalid.invalid")
	if err == nil {
		t.Fatal("expected error resolving invalid domain, got nil")
	}
}

func TestResolveReturnsIPv4ForLocalhost(t *testing.T) {
	addrs, err := net.LookupHost("localhost")
	if err != nil {
		t.Fatalf("failed to resolve localhost: %v", err)
	}
	hasIPv4 := false
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
			hasIPv4 = true
			break
		}
	}
	if !hasIPv4 {
		t.Fatal("expected at least one IPv4 address for localhost")
	}
}

func TestDialResolvesHostname(t *testing.T) {
	// Start a local TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// net.Dial with "localhost" should resolve and connect
	port := ln.Addr().(*net.TCPAddr).Port
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		t.Fatalf("failed to dial localhost:%d: %v", port, err)
	}
	conn.Close()
}

// --- Outbound bridge tests (ported from aws-nitro-enclaves-cli vsock_proxy/src/proxy.rs) ---

func TestBridgeToTCPHost(t *testing.T) {
	// Simulate the outbound proxy path: src → bridgeToTCPHost → target server
	// Uses TCP pairs since VSOCK is not available in CI.

	// Start a "remote server" that echoes data back
	remoteLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remoteLn.Close()

	go func() {
		conn, err := remoteLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // echo
	}()

	// Create a TCP connection pair to simulate the VSOCK side
	pairLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pairLn.Close()

	client, err := net.Dial("tcp", pairLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	src, err := pairLn.Accept()
	if err != nil {
		t.Fatal(err)
	}

	// Bridge the connection to the remote server
	done := make(chan error, 1)
	go func() {
		done <- bridgeToTCPHost(src, remoteLn.Addr().String())
	}()

	// Send data through and verify echo
	msg := "hello from enclave"
	fmt.Fprint(client, msg)
	client.(*net.TCPConn).CloseWrite()

	buf, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("expected %q, got %q", msg, string(buf))
	}

	if err := <-done; err != nil {
		t.Fatalf("bridge error: %v", err)
	}
}

// TestLargeDataTransfer is ported from aws-nitro-enclaves-cli vsock_proxy test_transfer.
// Verifies that data larger than io.Copy's internal buffer (32KB) transfers correctly.
func TestLargeDataTransfer(t *testing.T) {
	const dataSize = 1 << 20 // 1MB

	// Generate random data
	data := make([]byte, dataSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	// Start a "remote server" that reads everything and sends it back
	remoteLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remoteLn.Close()

	go func() {
		conn, err := remoteLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	// Create connection pair
	pairLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pairLn.Close()

	client, err := net.Dial("tcp", pairLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	src, err := pairLn.Accept()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- bridgeToTCPHost(src, remoteLn.Addr().String())
	}()

	// Write all data, then close write side
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	client.(*net.TCPConn).CloseWrite()

	// Read back and verify
	received, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != dataSize {
		t.Fatalf("expected %d bytes, got %d", dataSize, len(received))
	}
	for i := range data {
		if data[i] != received[i] {
			t.Fatalf("mismatch at byte %d: expected 0x%02x, got 0x%02x", i, data[i], received[i])
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("bridge error: %v", err)
	}
}

// TestConcurrentOutboundConnections verifies multiple simultaneous connections
// through the outbound bridge work correctly (mirrors vsock-proxy's worker pool).
func TestConcurrentOutboundConnections(t *testing.T) {
	// Start a "remote server" that echoes data
	remoteLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remoteLn.Close()

	go func() {
		for {
			conn, err := remoteLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()

	const numConns = 10
	var wg sync.WaitGroup
	errors := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Create connection pair
			pairLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				errors <- fmt.Errorf("conn %d: listen: %w", id, err)
				return
			}
			defer pairLn.Close()

			client, err := net.Dial("tcp", pairLn.Addr().String())
			if err != nil {
				errors <- fmt.Errorf("conn %d: dial: %w", id, err)
				return
			}

			src, err := pairLn.Accept()
			if err != nil {
				errors <- fmt.Errorf("conn %d: accept: %w", id, err)
				return
			}

			bridgeDone := make(chan error, 1)
			go func() {
				bridgeDone <- bridgeToTCPHost(src, remoteLn.Addr().String())
			}()

			msg := fmt.Sprintf("message from connection %d", id)
			fmt.Fprint(client, msg)
			client.(*net.TCPConn).CloseWrite()

			buf, err := io.ReadAll(client)
			if err != nil {
				errors <- fmt.Errorf("conn %d: read: %w", id, err)
				return
			}
			if string(buf) != msg {
				errors <- fmt.Errorf("conn %d: expected %q, got %q", id, msg, string(buf))
				return
			}

			if err := <-bridgeDone; err != nil {
				errors <- fmt.Errorf("conn %d: bridge: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

func TestCopyBidirectional(t *testing.T) {
	// Create two pairs of connected TCP sockets to test the bridge
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Client side
	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	serverConn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	// Create another pair
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()

	remoteConn, err := net.Dial("tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	backendConn, err := ln2.Accept()
	if err != nil {
		t.Fatal(err)
	}

	// Run bridge in background
	done := make(chan error, 1)
	go func() {
		done <- copyBidirectional(serverConn, remoteConn)
	}()

	// Send data through the bridge
	msg := "hello from client"
	fmt.Fprint(clientConn, msg)
	clientConn.(*net.TCPConn).CloseWrite()

	buf, err := io.ReadAll(backendConn)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("expected %q, got %q", msg, string(buf))
	}

	// Send response back
	resp := "hello from backend"
	fmt.Fprint(backendConn, resp)
	backendConn.Close()

	buf, err = io.ReadAll(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != resp {
		t.Fatalf("expected %q, got %q", resp, string(buf))
	}

	// Bridge should complete
	if err := <-done; err != nil {
		t.Fatalf("bridge error: %v", err)
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte(`{"httpPort":8080,"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[]}`))
	f.Add([]byte(`{"httpVsockPort":3000,"httpTcpPort":3000,"endpoints":[{"host":"sui.io","vsockPort":8443}]}`))
	f.Add([]byte(`{not json}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := ParseConfig(data)
		if err != nil {
			return
		}
		_ = ValidateConfig(cfg, validationModeHost)
		_ = ValidateConfig(cfg, validationModeEnclave)
	})
}
