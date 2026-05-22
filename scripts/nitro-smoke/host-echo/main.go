package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := "127.0.0.1:18443"
	if len(os.Args) == 2 {
		addr = os.Args[1]
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("[host-echo] listening on %s", addr)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-done
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[host-echo] accept: %v", err)
			continue
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		log.Printf("[host-echo] read: %v", err)
		return
	}
	if line != "outbound-ping\n" {
		log.Printf("[host-echo] unexpected request %q", line)
		return
	}
	if _, err := fmt.Fprint(conn, "outbound-pong\n"); err != nil {
		log.Printf("[host-echo] write: %v", err)
	}
}
