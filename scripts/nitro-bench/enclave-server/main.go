package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	argonautPath    = "/argonaut"
	configVsockPort = "7777"
	httpListenAddr  = "127.0.0.1:3000"
	maxPayloadBytes = 1 << 20
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Printf("ARGONAUT_BENCH_FAIL: %v", err)
		os.Exit(1)
	}
}

func run() error {
	config, err := receiveConfig()
	if err != nil {
		return err
	}
	log.Printf("[bench] received config (%d bytes)", len(config))

	server, err := startHTTPServer()
	if err != nil {
		return err
	}
	defer server.Shutdown(context.Background())

	argonaut, err := startArgonautEnclave(config)
	if err != nil {
		return err
	}
	defer stopProcess(argonaut)

	log.Println("ARGONAUT_BENCH_READY")
	select {
	case err := <-argonaut.exited:
		return fmt.Errorf("argonaut enclave exited: %w", err)
	}
}

func receiveConfig() ([]byte, error) {
	log.Println("ARGONAUT_BENCH_WAITING_FOR_CONFIG")
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

func startHTTPServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("argonaut-bench-ok"))
	})
	mux.HandleFunc("/bytes/", func(w http.ResponseWriter, r *http.Request) {
		rawSize := strings.TrimPrefix(r.URL.Path, "/bytes/")
		size, err := strconv.Atoi(rawSize)
		if err != nil || size < 0 || size > maxPayloadBytes {
			http.Error(w, "invalid size", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(size))
		writePayload(w, size)
	})

	ln, err := net.Listen("tcp", httpListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", httpListenAddr, err)
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[bench] http server error: %v", err)
		}
	}()
	return server, nil
}

func writePayload(w http.ResponseWriter, size int) {
	const chunkSize = 32 << 10
	var chunk [chunkSize]byte
	for i := range chunk {
		chunk[i] = byte('a' + i%26)
	}
	for size > 0 {
		n := size
		if n > len(chunk) {
			n = len(chunk)
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			return
		}
		size -= n
	}
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
	cmd.Env = append(os.Environ(), "ARGONAUT_LOG_CONNECTIONS=0")
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &runningProcess{cmd: cmd, exited: make(chan error, 1)}
	go func() {
		proc.exited <- cmd.Wait()
	}()
	return proc, nil
}

func stopProcess(proc *runningProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	_ = proc.cmd.Process.Kill()
}
