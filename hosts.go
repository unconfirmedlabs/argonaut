package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

const (
	hostsPath        = "/etc/hosts"
	hostsBeginMarker = "# argonaut begin"
	hostsEndMarker   = "# argonaut end"
)

func renderHosts(endpoints []Endpoint) ([]byte, error) {
	return mergeHosts([]byte("127.0.0.1   localhost\n"), endpoints)
}

func mergeHosts(existing []byte, endpoints []Endpoint) ([]byte, error) {
	for _, ep := range endpoints {
		if err := ValidateEndpoint(ep); err != nil {
			return nil, err
		}
	}

	base := stripManagedHostsBlock(string(existing))
	base = strings.TrimRight(base, "\n")
	if strings.TrimSpace(base) == "" {
		base = "127.0.0.1   localhost"
	}

	var block strings.Builder
	block.WriteString(hostsBeginMarker)
	block.WriteByte('\n')
	for i, ep := range endpoints {
		fmt.Fprintf(&block, "%s   %s\n", endpointLocalIP(ep, i), ep.Host)
	}
	block.WriteString(hostsEndMarker)
	block.WriteByte('\n')

	var out bytes.Buffer
	out.WriteString(base)
	out.WriteString("\n\n")
	out.WriteString(block.String())
	return out.Bytes(), nil
}

func stripManagedHostsBlock(existing string) string {
	begin := strings.Index(existing, hostsBeginMarker)
	if begin == -1 {
		return existing
	}
	endRel := strings.Index(existing[begin:], hostsEndMarker)
	if endRel == -1 {
		return strings.TrimRight(existing[:begin], "\n") + "\n"
	}
	end := begin + endRel + len(hostsEndMarker)
	for end < len(existing) && (existing[end] == '\n' || existing[end] == '\r') {
		end++
	}
	return existing[:begin] + existing[end:]
}

func writeHosts(path string, endpoints []Endpoint) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	content, err := mergeHosts(existing, endpoints)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
