package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

const (
	schemaVersionV01 = "v0.1"
	maxConfigBytes   = 1 << 20
	maxEndpoints     = 191
	defaultTCPPort   = uint16(443)
)

type validationMode string

const (
	validationModeHost    validationMode = "host"
	validationModeEnclave validationMode = "enclave"
)

// Config is the public v0.1 argonaut configuration schema.
//
// httpPort is required by host mode. httpVsockPort is required by both host and
// enclave modes. httpTcpPort is required by enclave mode. Extra top-level fields
// are intentionally tolerated so application config can travel through host mode
// to the enclave unchanged.
type Config struct {
	SchemaVersion string     `json:"schemaVersion,omitempty"`
	HTTPPort      uint16     `json:"httpPort,omitempty"`
	HTTPVsockPort uint32     `json:"httpVsockPort"`
	HTTPTCPPort   uint16     `json:"httpTcpPort,omitempty"`
	Endpoints     []Endpoint `json:"endpoints"`

	endpointsSet bool
}

type Endpoint struct {
	Host      string `json:"host"`
	VsockPort uint32 `json:"vsockPort"`
	TCPPort   uint16 `json:"tcpPort,omitempty"`
	LocalPort uint16 `json:"localPort,omitempty"`
	LocalIP   string `json:"localIP,omitempty"`

	hostSet      bool
	vsockPortSet bool
	tcpPortSet   bool
	localPortSet bool
	localIPSet   bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configAlias Config
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if err := ensureNoTrailingJSON(dec); err != nil {
		return err
	}

	var alias configAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*c = Config(alias)
	_, c.endpointsSet = raw["endpoints"]
	return nil
}

func (e *Endpoint) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if err := ensureNoTrailingJSON(dec); err != nil {
		return err
	}

	if v, ok := raw["host"]; ok {
		e.hostSet = true
		if err := json.Unmarshal(v, &e.Host); err != nil {
			return fmt.Errorf("host: %w", err)
		}
	}
	if v, ok := raw["vsockPort"]; ok {
		e.vsockPortSet = true
		if err := json.Unmarshal(v, &e.VsockPort); err != nil {
			return fmt.Errorf("vsockPort: %w", err)
		}
	}
	if v, ok := raw["tcpPort"]; ok {
		e.tcpPortSet = true
		if err := json.Unmarshal(v, &e.TCPPort); err != nil {
			return fmt.Errorf("tcpPort: %w", err)
		}
	}
	if v, ok := raw["localPort"]; ok {
		e.localPortSet = true
		if err := json.Unmarshal(v, &e.LocalPort); err != nil {
			return fmt.Errorf("localPort: %w", err)
		}
	}
	if v, ok := raw["localIP"]; ok {
		e.localIPSet = true
		if err := json.Unmarshal(v, &e.LocalIP); err != nil {
			return fmt.Errorf("localIP: %w", err)
		}
	}
	return nil
}

func ParseConfig(data []byte) (*Config, error) {
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config too large (%d > %d)", len(data), maxConfigBytes)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	if err := ensureNoTrailingJSON(dec); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	normalizeConfig(&cfg)
	return &cfg, nil
}

func ensureNoTrailingJSON(dec *json.Decoder) error {
	var extra struct{}
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("trailing JSON value")
}

func normalizeConfig(cfg *Config) {
	if cfg.SchemaVersion == "" {
		cfg.SchemaVersion = schemaVersionV01
	}
	for i := range cfg.Endpoints {
		if !cfg.Endpoints[i].tcpPortSet {
			cfg.Endpoints[i].TCPPort = defaultTCPPort
		}
		if !cfg.Endpoints[i].localPortSet {
			cfg.Endpoints[i].LocalPort = defaultTCPPort
		}
	}
}

func ValidateConfig(cfg *Config, mode validationMode) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	normalizeConfig(cfg)
	if cfg.SchemaVersion != schemaVersionV01 {
		return fmt.Errorf("unsupported schemaVersion %q", cfg.SchemaVersion)
	}
	if !cfg.endpointsSet || cfg.Endpoints == nil {
		return fmt.Errorf("endpoints must be an array")
	}
	if len(cfg.Endpoints) > maxEndpoints {
		return fmt.Errorf("too many endpoints (max %d, got %d)", maxEndpoints, len(cfg.Endpoints))
	}

	switch mode {
	case validationModeHost:
		if cfg.HTTPPort == 0 {
			return fmt.Errorf("httpPort must be an integer in 1..65535")
		}
		if cfg.HTTPVsockPort == 0 || cfg.HTTPVsockPort > 65535 {
			return fmt.Errorf("httpVsockPort must be an integer in 1..65535")
		}
	case validationModeEnclave:
		if cfg.HTTPVsockPort == 0 || cfg.HTTPVsockPort > 65535 {
			return fmt.Errorf("httpVsockPort must be an integer in 1..65535")
		}
		if cfg.HTTPTCPPort == 0 {
			return fmt.Errorf("httpTcpPort must be an integer in 1..65535")
		}
	default:
		return fmt.Errorf("unknown config mode %q", mode)
	}

	hosts := make(map[string]int, len(cfg.Endpoints))
	vsockPorts := make(map[uint32]int, len(cfg.Endpoints))
	localAddrs := make(map[string]int, len(cfg.Endpoints))
	for i := range cfg.Endpoints {
		if err := ValidateEndpoint(cfg.Endpoints[i]); err != nil {
			return fmt.Errorf("endpoints[%d]: %w", i, err)
		}

		hostKey := strings.ToLower(cfg.Endpoints[i].Host)
		if prev, ok := hosts[hostKey]; ok {
			return fmt.Errorf("endpoints[%d].host duplicates endpoints[%d].host", i, prev)
		}
		hosts[hostKey] = i

		if prev, ok := vsockPorts[cfg.Endpoints[i].VsockPort]; ok {
			return fmt.Errorf("endpoints[%d].vsockPort duplicates endpoints[%d].vsockPort", i, prev)
		}
		vsockPorts[cfg.Endpoints[i].VsockPort] = i

		localAddr := endpointLocalIP(cfg.Endpoints[i], i) + ":" + fmt.Sprint(cfg.Endpoints[i].LocalPort)
		if prev, ok := localAddrs[localAddr]; ok {
			return fmt.Errorf("endpoints[%d] local bind address duplicates endpoints[%d]", i, prev)
		}
		localAddrs[localAddr] = i
	}
	return nil
}

func ValidateEndpoint(ep Endpoint) error {
	if ep.Host == "" {
		return fmt.Errorf("host must be a non-empty string")
	}
	if len(ep.Host) > 253 {
		return fmt.Errorf("host must be <=253 characters")
	}
	if err := validateHostname(ep.Host); err != nil {
		return fmt.Errorf("host contains invalid characters: %w", err)
	}
	if ep.VsockPort == 0 || ep.VsockPort > 65535 {
		return fmt.Errorf("vsockPort must be an integer in 1..65535")
	}
	if ep.tcpPortSet && ep.TCPPort == 0 {
		return fmt.Errorf("tcpPort must be an integer in 1..65535")
	}
	if ep.localPortSet && ep.LocalPort == 0 {
		return fmt.Errorf("localPort must be an integer in 1..65535")
	}
	if ep.localIPSet && ep.LocalIP == "" {
		return fmt.Errorf("localIP must be a non-empty string")
	}
	if ep.LocalIP != "" {
		ip, err := netip.ParseAddr(ep.LocalIP)
		if err != nil || !ip.Is4() || !ip.IsLoopback() {
			return fmt.Errorf("localIP must be an IPv4 loopback address")
		}
	}
	return nil
}

func endpointLocalIP(ep Endpoint, index int) string {
	if ep.LocalIP != "" {
		return ep.LocalIP
	}
	return fmt.Sprintf("127.0.0.%d", 64+index)
}

func validateHostname(host string) error {
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("%q", r)
		}
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("label too long")
		}
	}
	return nil
}

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input too large (%d > %d)", len(data), limit)
	}
	return data, nil
}
