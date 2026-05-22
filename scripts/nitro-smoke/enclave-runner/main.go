package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	argonautPath    = "/argonaut"
	configVsockPort = "7777"
	httpListenAddr  = "127.0.0.1:3000"
)

type nsmResponse struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Data  string `json:"data"`
	Error string `json:"error"`
}

type smokeConfig struct {
	Endpoints []smokeEndpoint `json:"endpoints"`
}

type smokeEndpoint struct {
	Host      string `json:"host"`
	LocalIP   string `json:"localIP"`
	LocalPort uint16 `json:"localPort"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Printf("ARGONAUT_SMOKE_FAIL: %v", err)
		os.Exit(1)
	}
	log.Println("ARGONAUT_SMOKE_OK")
}

func run() error {
	config, err := receiveConfig()
	if err != nil {
		return err
	}
	log.Printf("[smoke] received config (%d bytes)", len(config))
	endpoint, err := parseSmokeEndpoint(config)
	if err != nil {
		return err
	}

	if err := testNSM(); err != nil {
		return fmt.Errorf("nsm: %w", err)
	}
	log.Println("[smoke] nsm ok")

	inboundHit := make(chan struct{}, 1)
	httpServer, err := startHTTPServer(inboundHit)
	if err != nil {
		return err
	}
	defer httpServer.Shutdown(context.Background())

	argonaut, err := startArgonautEnclave(config)
	if err != nil {
		return err
	}
	defer stopProcess(argonaut)

	if err := verifyHostsBlock(argonaut.exited, endpoint); err != nil {
		return fmt.Errorf("hosts file: %w", err)
	}
	log.Println("[smoke] hosts file ok")

	if err := testOutbound(argonaut.exited, endpoint.outboundAddr()); err != nil {
		return fmt.Errorf("outbound bridge: %w", err)
	}
	log.Println("[smoke] outbound bridge ok")

	log.Println("ARGONAUT_SMOKE_READY_FOR_INBOUND")
	select {
	case <-inboundHit:
		log.Println("[smoke] inbound bridge ok")
	case err := <-argonaut.exited:
		return fmt.Errorf("argonaut enclave exited before inbound test: %w", err)
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timed out waiting for inbound bridge request")
	}

	return nil
}

func receiveConfig() ([]byte, error) {
	log.Println("ARGONAUT_SMOKE_WAITING_FOR_CONFIG")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, argonautPath, "config", "recv", configVsockPort)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("config recv: %w", err)
	}
	return out.Bytes(), nil
}

func parseSmokeEndpoint(config []byte) (smokeEndpoint, error) {
	var cfg smokeConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return smokeEndpoint{}, err
	}
	if len(cfg.Endpoints) != 1 {
		return smokeEndpoint{}, fmt.Errorf("expected exactly 1 smoke endpoint, got %d", len(cfg.Endpoints))
	}
	ep := cfg.Endpoints[0]
	if ep.Host == "" {
		return smokeEndpoint{}, fmt.Errorf("smoke endpoint host is empty")
	}
	if ep.LocalIP == "" {
		ep.LocalIP = "127.0.0.64"
	}
	if ep.LocalPort == 0 {
		ep.LocalPort = 443
	}
	return ep, nil
}

func (ep smokeEndpoint) outboundAddr() string {
	return net.JoinHostPort(ep.LocalIP, fmt.Sprint(ep.LocalPort))
}

func testNSM() error {
	if _, err := os.Stat("/dev/nsm"); err != nil {
		return fmt.Errorf("stat /dev/nsm: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, argonautPath, "nsm")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer stopProcess(&runningProcess{cmd: cmd})

	reader := bufio.NewReader(stdout)
	random, err := nsmRequest(ctx, stdin, reader, `{"id":"rnd","method":"RND"}`)
	if err != nil {
		return err
	}
	if random.ID != "rnd" || !random.OK || random.Data == "" {
		return fmt.Errorf("bad RND response: %+v", random)
	}

	attReq := `{"id":"att","method":"ATT","publicKey":"` + strings.Repeat("11", 32) + `","nonce":"aabbccdd","userData":"01020304"}`
	att, err := nsmRequest(ctx, stdin, reader, attReq)
	if err != nil {
		return err
	}
	if att.ID != "att" || !att.OK || len(att.Data) < 64 {
		return fmt.Errorf("bad ATT response: %+v", att)
	}

	return nil
}

func nsmRequest(ctx context.Context, stdin io.Writer, reader *bufio.Reader, request string) (nsmResponse, error) {
	if _, err := fmt.Fprintln(stdin, request); err != nil {
		return nsmResponse{}, err
	}

	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- result{line: strings.TrimSpace(line), err: err}
	}()

	select {
	case <-ctx.Done():
		return nsmResponse{}, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return nsmResponse{}, res.err
		}
		var resp nsmResponse
		if err := json.Unmarshal([]byte(res.line), &resp); err != nil {
			return nsmResponse{}, fmt.Errorf("decode %q: %w", res.line, err)
		}
		if !resp.OK {
			return resp, fmt.Errorf("nsm error: %s", resp.Error)
		}
		return resp, nil
	}
}

func startHTTPServer(hit chan<- struct{}) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("argonaut-inbound-ok"))
	})

	ln, err := net.Listen("tcp", httpListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", httpListenAddr, err)
	}

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[smoke] http server error: %v", err)
		}
	}()
	return server, nil
}

type runningProcess struct {
	cmd    *exec.Cmd
	exited chan error
}

func startArgonautEnclave(config []byte) (*runningProcess, error) {
	cmd := exec.Command(argonautPath, "enclave")
	cmd.Stdin = bytes.NewReader(config)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &runningProcess{cmd: cmd, exited: make(chan error, 1)}
	go func() {
		proc.exited <- cmd.Wait()
	}()
	return proc, nil
}

func verifyHostsBlock(exited <-chan error, ep smokeEndpoint) error {
	deadline := time.After(10 * time.Second)
	expected := ep.LocalIP + "   " + ep.Host
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("argonaut enclave exited: %w", err)
		case <-deadline:
			return fmt.Errorf("timed out waiting for %q in /etc/hosts", expected)
		default:
		}

		content, err := os.ReadFile("/etc/hosts")
		if err == nil && strings.Contains(string(content), hostsBeginMarker()) && strings.Contains(string(content), expected) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func hostsBeginMarker() string {
	return "# argonaut begin"
}

func testOutbound(exited <-chan error, outboundProxyAddr string) error {
	deadline := time.After(60 * time.Second)
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("argonaut enclave exited: %w", err)
		case <-deadline:
			return fmt.Errorf("timed out connecting to %s", outboundProxyAddr)
		default:
		}

		conn, err := net.DialTimeout("tcp", outboundProxyAddr, time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			conn.Close()
			return err
		}
		if _, err := conn.Write([]byte("outbound-ping\n")); err != nil {
			conn.Close()
			return err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil {
			return err
		}
		if line != "outbound-pong\n" {
			return fmt.Errorf("got %q, want outbound-pong", line)
		}
		return nil
	}
}

func stopProcess(proc *runningProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	_ = proc.cmd.Process.Kill()
}
