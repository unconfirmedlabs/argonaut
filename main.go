// Argonaut — companion binary for AWS Nitro Enclaves.
//
// Handles VSOCK↔TCP bridging, config delivery, and NSM attestation.
// Named "arGOnaut" as a nod to Go.
//
// Modes:
//
//	argonaut host <cid> <config-file>
//	    Read JSON config, send it to the enclave via VSOCK:7777, then:
//	    1. Inbound:  TCP listen → VSOCK connect (HTTP into the enclave)
//	    2. Outbound: VSOCK listen → TCP connect (enclave reaching external services)
//	    Replaces both the separate config-send step and AWS vsock-proxy.
//
//	argonaut enclave
//	    Read JSON config from stdin, then:
//	    1. Write /etc/hosts for endpoint hostname resolution
//	    2. Inbound:  VSOCK listen → TCP connect to localhost (HTTP runtime)
//	    3. Outbound: TCP listen on loopback → VSOCK connect to parent
//	    Runs inside the Nitro Enclave where there is no network.
//
//	argonaut config send <cid> <vsock-port>
//	    Read stdin and send it to VSOCK:<cid>:<port>. Low-level utility
//	    for debugging; normal usage goes through "argonaut host".
//
//	argonaut config recv <vsock-port>
//	    Listen on VSOCK:<port>, accept one connection, read all data,
//	    and write it to stdout. Used inside the enclave at boot.
//
//	argonaut nsm
//	    NSM proxy: stdin/stdout JSON Lines protocol for attestation and RNG.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdlayher/vsock"
)

const parentCID = 3
const defaultMaxConcurrentConnections = 1024

var connectionIDs atomic.Uint64
var configuredMaxConcurrentConnections = envInt("ARGONAUT_MAX_CONNECTIONS", defaultMaxConcurrentConnections)
var logBridgeConnections = envBool("ARGONAUT_LOG_CONNECTIONS", true)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "host":
		hostMode()
	case "enclave":
		enclaveMode()
	case "config":
		configMode()
	case "nsm":
		nsmMode()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  %s host <cid> <config-file>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s enclave  (reads JSON config from stdin)\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s config send <cid> <vsock-port>  (send stdin to VSOCK)\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s config recv <vsock-port>        (receive from VSOCK to stdout)\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s nsm                             (NSM proxy: stdin/stdout line protocol)\n", os.Args[0])
	os.Exit(1)
}

// --- Host mode: config delivery + inbound/outbound bridges ---

const configVsockPort uint32 = 7777

func hostMode() {
	if len(os.Args) != 4 {
		usage()
	}

	cid, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil {
		log.Fatalf("invalid enclave CID: %s", os.Args[2])
	}

	configPath := os.Args[3]
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("failed to read config file: %v", err)
	}

	hostCfg, err := ParseConfig(rawConfig)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := ValidateConfig(hostCfg, validationModeHost); err != nil {
		log.Fatalf("invalid host config: %v", err)
	}

	// Send config to enclave via VSOCK:7777
	sendConfigVSOCK(uint32(cid), configVsockPort, rawConfig)

	// Start all bridges
	var wg sync.WaitGroup

	// Inbound: TCP:<httpPort> → VSOCK:<cid>:<httpVsockPort>
	wg.Add(1)
	go func() {
		defer wg.Done()
		hostInboundBridge(hostCfg.HTTPPort, uint32(cid), hostCfg.HTTPVsockPort)
	}()

	// Outbound: for each endpoint, VSOCK:<vsockPort> → TCP:<host>:<tcpPort>
	for _, ep := range hostCfg.Endpoints {
		wg.Add(1)
		go func(ep Endpoint) {
			defer wg.Done()
			hostOutboundBridge(ep)
		}(ep)
	}

	log.Println("[host] all bridges started")
	wg.Wait()
	log.Println("[host] bridge exited unexpectedly")
	os.Exit(1)
}

// sendConfigVSOCK sends raw bytes to a VSOCK endpoint.
func sendConfigVSOCK(cid, port uint32, data []byte) {
	conn, err := vsock.Dial(cid, port, nil)
	if err != nil {
		log.Fatalf("[host] VSOCK dial %d:%d for config: %v", cid, port, err)
	}
	if err := writeAll(conn, data); err != nil {
		conn.Close()
		log.Fatalf("[host] config write: %v", err)
	}
	conn.Close()
	log.Printf("[host] config sent (%d bytes) to CID %d via VSOCK:%d", len(data), cid, port)
}

func hostInboundBridge(tcpPort uint16, cid, vsockPort uint32) {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", tcpPort))
	if err != nil {
		log.Fatalf("[host] failed to listen on TCP:%d: %v", tcpPort, err)
	}
	log.Printf("[host] inbound TCP:%d → VSOCK:%d:%d", tcpPort, cid, vsockPort)
	limiter := newConnectionLimiter(configuredConnectionLimit())

	for {
		tcp, err := ln.Accept()
		if err != nil {
			log.Printf("[host] inbound accept error: %v", err)
			continue
		}
		if !limiter.tryAcquire() {
			log.Printf("[host] inbound connection limit reached; closing %s", tcp.RemoteAddr())
			tcp.Close()
			continue
		}
		go func() {
			defer limiter.release()
			if err := bridgeToVSOCK(tcp, cid, vsockPort); err != nil {
				log.Printf("[host] inbound bridge error: %v", err)
			}
		}()
	}
}

// hostOutboundBridge listens on a VSOCK port for connections from the enclave
// and bridges them to the endpoint's TCP host:443. This replaces AWS vsock-proxy.
func hostOutboundBridge(ep Endpoint) {
	ln, err := vsock.Listen(ep.VsockPort, nil)
	if err != nil {
		log.Fatalf("[host] failed to listen on VSOCK:%d for %s: %v", ep.VsockPort, ep.Host, err)
	}

	target := net.JoinHostPort(ep.Host, strconv.Itoa(int(ep.TCPPort)))
	log.Printf("[host] outbound VSOCK:%d → TCP:%s", ep.VsockPort, target)
	limiter := newConnectionLimiter(configuredConnectionLimit())

	for {
		vc, err := ln.Accept()
		if err != nil {
			log.Printf("[host] outbound accept error (VSOCK:%d): %v", ep.VsockPort, err)
			continue
		}
		if !limiter.tryAcquire() {
			log.Printf("[host] outbound connection limit reached (VSOCK:%d); closing %s", ep.VsockPort, vc.RemoteAddr())
			vc.Close()
			continue
		}
		go func() {
			defer limiter.release()
			if err := bridgeToTCPHost(vc, target); err != nil {
				log.Printf("[host] outbound bridge error (%s): %v", target, err)
			}
		}()
	}
}

// bridgeToTCPHost bridges a connection to a remote TCP host (DNS resolved at dial time).
func bridgeToTCPHost(src net.Conn, target string) error {
	id := nextConnectionID()
	start := time.Now()
	defer src.Close()

	logConnectionf("[bridge:%d] %s → TCP:%s accepted", id, src.RemoteAddr(), target)
	tcp, err := (&net.Dialer{Timeout: 30 * time.Second}).Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("TCP dial %s: %w", target, err)
	}
	defer tcp.Close()

	result := copyBidirectionalResult(src, tcp)
	logConnectionf("[bridge:%d] closed target=TCP:%s duration=%s a_to_b=%d b_to_a=%d err_a_to_b=%v err_b_to_a=%v",
		id, target, time.Since(start), result.AToBBytes, result.BToABytes, result.AToBErr, result.BToAErr)
	return result.Err()
}

func bridgeToVSOCK(src net.Conn, cid, port uint32) error {
	id := nextConnectionID()
	start := time.Now()
	defer src.Close()

	logConnectionf("[bridge:%d] %s → VSOCK:%d:%d accepted", id, src.RemoteAddr(), cid, port)
	vc, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return fmt.Errorf("VSOCK dial %d:%d: %w", cid, port, err)
	}
	defer vc.Close()

	result := copyBidirectionalResult(src, vc)
	logConnectionf("[bridge:%d] closed target=VSOCK:%d:%d duration=%s a_to_b=%d b_to_a=%d err_a_to_b=%v err_b_to_a=%v",
		id, cid, port, time.Since(start), result.AToBBytes, result.BToABytes, result.AToBErr, result.BToAErr)
	return result.Err()
}

// --- Enclave mode: JSON config from stdin, multiple bridges ---

func enclaveMode() {
	rawConfig, err := readAllLimited(os.Stdin, maxConfigBytes)
	if err != nil {
		log.Fatalf("failed to read config: %v", err)
	}
	config, err := ParseConfig(rawConfig)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := ValidateConfig(config, validationModeEnclave); err != nil {
		log.Fatalf("invalid enclave config: %v", err)
	}

	if err := ensureLoopbackUp(); err != nil {
		log.Fatalf("[traffic] could not bring loopback interface up: %v", err)
	}

	if err := writeHosts(hostsPath, config.Endpoints); err != nil {
		log.Fatalf("[traffic] could not write %s: %v", hostsPath, err)
	}

	// All bridges run forever — if any returns, something failed.
	var wg sync.WaitGroup

	// Inbound: VSOCK listen → TCP connect (HTTP traffic to Bun)
	wg.Add(1)
	go func() {
		defer wg.Done()
		inboundBridge(config.HTTPVsockPort, config.HTTPTCPPort)
	}()

	// Outbound: TCP listen → VSOCK connect (external services)
	for i, ep := range config.Endpoints {
		ip := endpointLocalIP(ep, i)
		log.Printf("[traffic] %s → %s:%d → VSOCK:%d:%d", ep.Host, ip, ep.LocalPort, parentCID, ep.VsockPort)

		wg.Add(1)
		go func(localIP string, ep Endpoint) {
			defer wg.Done()
			outboundProxy(localIP, ep)
		}(ip, ep)
	}

	log.Println("[traffic] ready")
	wg.Wait()
	log.Println("[traffic] bridge exited unexpectedly")
	os.Exit(1)
}

func inboundBridge(vsockPort uint32, tcpPort uint16) {
	ln, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Fatalf("[traffic] failed to bind VSOCK:%d: %v", vsockPort, err)
	}
	log.Printf("[traffic] inbound VSOCK:%d → TCP:127.0.0.1:%d", vsockPort, tcpPort)
	limiter := newConnectionLimiter(configuredConnectionLimit())

	for {
		vc, err := ln.Accept()
		if err != nil {
			log.Printf("[traffic] inbound accept error: %v", err)
			continue
		}
		if !limiter.tryAcquire() {
			log.Printf("[traffic] inbound connection limit reached; closing %s", vc.RemoteAddr())
			vc.Close()
			continue
		}
		go func() {
			defer limiter.release()
			if err := bridgeToTCP(vc, tcpPort); err != nil {
				log.Printf("[traffic] inbound bridge error: %v", err)
			}
		}()
	}
}

func bridgeToTCP(src net.Conn, tcpPort uint16) error {
	id := nextConnectionID()
	start := time.Now()
	defer src.Close()

	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(tcpPort)))
	logConnectionf("[bridge:%d] %s → TCP:%s accepted", id, src.RemoteAddr(), target)
	tcp, err := (&net.Dialer{Timeout: 30 * time.Second}).Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("TCP dial 127.0.0.1:%d: %w", tcpPort, err)
	}
	defer tcp.Close()

	result := copyBidirectionalResult(src, tcp)
	logConnectionf("[bridge:%d] closed target=TCP:%s duration=%s a_to_b=%d b_to_a=%d err_a_to_b=%v err_b_to_a=%v",
		id, target, time.Since(start), result.AToBBytes, result.BToABytes, result.AToBErr, result.BToAErr)
	return result.Err()
}

func outboundProxy(localIP string, ep Endpoint) {
	addr := net.JoinHostPort(localIP, strconv.Itoa(int(ep.LocalPort)))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[traffic] failed to bind %s: %v", addr, err)
	}
	log.Printf("[traffic] outbound TCP:%s → VSOCK:%d:%d", addr, parentCID, ep.VsockPort)
	limiter := newConnectionLimiter(configuredConnectionLimit())

	for {
		tcp, err := ln.Accept()
		if err != nil {
			log.Printf("[traffic] outbound accept error: %v", err)
			continue
		}
		if !limiter.tryAcquire() {
			log.Printf("[traffic] outbound connection limit reached; closing %s", tcp.RemoteAddr())
			tcp.Close()
			continue
		}
		go func() {
			defer limiter.release()
			if err := bridgeToVSOCK(tcp, parentCID, ep.VsockPort); err != nil {
				log.Printf("[traffic] outbound bridge error: %v", err)
			}
		}()
	}
}

// --- Config mode: one-shot VSOCK send/recv ---

func configMode() {
	if len(os.Args) < 4 {
		usage()
	}

	switch os.Args[2] {
	case "send":
		configSend()
	case "recv":
		configRecv()
	default:
		usage()
	}
}

func configSend() {
	if len(os.Args) != 5 {
		usage()
	}

	cid, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		log.Fatalf("invalid CID: %s", os.Args[3])
	}
	port, err := strconv.ParseUint(os.Args[4], 10, 32)
	if err != nil {
		log.Fatalf("invalid VSOCK port: %s", os.Args[4])
	}

	data, err := readAllLimited(os.Stdin, maxConfigBytes)
	if err != nil {
		log.Fatalf("failed to read stdin: %v", err)
	}

	sendConfigVSOCK(uint32(cid), uint32(port), data)
}

func configRecv() {
	if len(os.Args) != 4 {
		usage()
	}

	port, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		log.Fatalf("invalid VSOCK port: %s", os.Args[3])
	}

	ln, err := vsock.Listen(uint32(port), nil)
	if err != nil {
		log.Fatalf("VSOCK listen :%d: %v", port, err)
	}

	conn, err := ln.Accept()
	if err != nil {
		log.Fatalf("VSOCK accept: %v", err)
	}
	ln.Close()

	data, err := readAllLimited(conn, maxConfigBytes)
	if err != nil {
		conn.Close()
		log.Fatalf("VSOCK read: %v", err)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		conn.Close()
		log.Fatalf("stdout write: %v", err)
	}
	conn.Close()
}

// --- Shared bidirectional copy with proper half-close ---

func copyBidirectional(a, b net.Conn) error {
	return copyBidirectionalResult(a, b).Err()
}

type copyResult struct {
	AToBBytes int64
	BToABytes int64
	AToBErr   error
	BToAErr   error
}

func (r copyResult) Err() error {
	if r.AToBErr != nil {
		return r.AToBErr
	}
	return r.BToAErr
}

func copyBidirectionalResult(a, b net.Conn) copyResult {
	type copyOutcome struct {
		bytes int64
		err   error
		aToB  bool
	}
	done := make(chan copyOutcome, 2)

	go func() {
		n, err := io.Copy(b, a)
		closeWrite(b)
		done <- copyOutcome{bytes: n, err: err, aToB: true}
	}()

	go func() {
		n, err := io.Copy(a, b)
		closeWrite(a)
		done <- copyOutcome{bytes: n, err: err, aToB: false}
	}()

	// Wait for both directions
	first := <-done
	second := <-done

	var result copyResult
	for _, outcome := range []copyOutcome{first, second} {
		if outcome.aToB {
			result.AToBBytes = outcome.bytes
			result.AToBErr = outcome.err
		} else {
			result.BToABytes = outcome.bytes
			result.BToAErr = outcome.err
		}
	}
	return result
}

// closeWrite calls CloseWrite on connections that support it (TCP and VSOCK).
func closeWrite(c net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := c.(halfCloser); ok {
		hc.CloseWrite()
	}
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func configuredConnectionLimit() int {
	return configuredMaxConcurrentConnections
}

func logConnectionf(format string, args ...interface{}) {
	if logBridgeConnections {
		log.Printf(format, args...)
	}
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Printf("invalid %s=%q; using %d", name, value, fallback)
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		log.Printf("invalid %s=%q; using %t", name, value, fallback)
		return fallback
	}
	return parsed
}

type connectionLimiter struct {
	sem chan struct{}
}

func newConnectionLimiter(max int) *connectionLimiter {
	return &connectionLimiter{sem: make(chan struct{}, max)}
}

func (l *connectionLimiter) tryAcquire() bool {
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *connectionLimiter) release() {
	<-l.sem
}

func nextConnectionID() uint64 {
	return connectionIDs.Add(1)
}
