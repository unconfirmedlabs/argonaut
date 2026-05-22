// NSM (Nitro Secure Module) interface for /dev/nsm.
//
// Implements the ioctl interface, CBOR request/response encoding,
// and stdin/stdout protocols for runtime clients.
//
// JSON Lines protocol:
//
//	{"id":"1","method":"ATT","publicKey":"<hex>","nonce":"<hex>","userData":"<hex>"}
//	{"id":"1","ok":true,"data":"<hex-attestation-doc>"}
//	{"id":"2","method":"RND"}
//	{"id":"2","ok":true,"data":"<hex-random-bytes>"}
//
// A legacy space-delimited protocol is still accepted for existing callers:
//
//	<id> ATT <hex-public-key>
//	<id> ATT <hex-public-key> <hex-nonce|-> <hex-user-data|->
//	<id> RND
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/sys/unix"
)

const (
	nsmDevicePath      = "/dev/nsm"
	nsmRequestMaxSize  = 0x1000 // 4096 bytes
	nsmResponseMaxSize = 0x3000 // 12288 bytes
	nsmLineMaxSize     = 1 << 20
	nsmIDMaxSize       = 64
	nsmFieldMaxSize    = 2048

	// _IOWR(0x0A, 0, 32) on x86_64
	// Direction=ReadWrite(3) << 30 | Size(32) << 16 | Type(0x0A) << 8 | Nr(0)
	nsmIoctlRequest = 0xC0200A00
)

// iovec matches the Linux struct iovec layout on x86_64.
type iovec struct {
	Base uintptr
	Len  uint64
}

// nsmMessage matches the kernel struct nsm_message (two iovecs, 32 bytes total).
type nsmMessage struct {
	Request  iovec
	Response iovec
}

// --- NsmBackend interface for testability ---

// NsmBackend abstracts NSM operations for testing.
type NsmBackend interface {
	GetAttestation(req AttestationRequest) ([]byte, error)
	GetRandom() ([]byte, error)
}

type AttestationRequest struct {
	PublicKey []byte
	Nonce     []byte
	UserData  []byte
}

// --- NitroBackend: real /dev/nsm ioctl ---

// NitroBackend talks to the real NSM device via ioctl.
type NitroBackend struct {
	fd int
}

// OpenNsm opens /dev/nsm and returns a backend.
func OpenNsm() (*NitroBackend, error) {
	fd, err := unix.Open(nsmDevicePath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsmDevicePath, err)
	}
	return &NitroBackend{fd: fd}, nil
}

// Close releases the file descriptor.
func (n *NitroBackend) Close() {
	unix.Close(n.fd)
}

func (n *NitroBackend) GetAttestation(req AttestationRequest) ([]byte, error) {
	if err := validateAttestationRequest(req); err != nil {
		return nil, err
	}

	encodedReq, err := encodeAttestationRequest(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	resp, err := nsmIoctl(n.fd, encodedReq)
	if err != nil {
		return nil, err
	}

	return decodeAttestationResponse(resp)
}

func (n *NitroBackend) GetRandom() ([]byte, error) {
	req, err := encodeGetRandomRequest()
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	resp, err := nsmIoctl(n.fd, req)
	if err != nil {
		return nil, err
	}

	return decodeGetRandomResponse(resp)
}

// --- ioctl ---

func nsmIoctl(fd int, request []byte) ([]byte, error) {
	if len(request) == 0 {
		return nil, fmt.Errorf("empty request")
	}
	if len(request) > nsmRequestMaxSize {
		return nil, fmt.Errorf("request too large (%d > %d)", len(request), nsmRequestMaxSize)
	}

	response := make([]byte, nsmResponseMaxSize)

	msg := nsmMessage{
		Request: iovec{
			Base: uintptr(unsafe.Pointer(&request[0])),
			Len:  uint64(len(request)),
		},
		Response: iovec{
			Base: uintptr(unsafe.Pointer(&response[0])),
			Len:  uint64(len(response)),
		},
	}

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(nsmIoctlRequest),
		uintptr(unsafe.Pointer(&msg)),
	)

	// Keep buffers alive across the syscall
	runtime.KeepAlive(request)
	runtime.KeepAlive(response)

	if errno != 0 {
		if errno == unix.EMSGSIZE {
			return nil, fmt.Errorf("input too large")
		}
		return nil, fmt.Errorf("ioctl failed: %v", errno)
	}
	if msg.Response.Len > uint64(len(response)) {
		return nil, fmt.Errorf("response too large (%d > %d)", msg.Response.Len, len(response))
	}

	return response[:msg.Response.Len], nil
}

// --- CBOR encoding/decoding ---
//
// The AWS NSM API uses serde's externally-tagged enum format:
//   - Unit variants: CBOR text string, e.g. "GetRandom"
//   - Struct variants: CBOR map, e.g. {"Attestation": {"public_key": <bytes>, ...}}

func validateAttestationRequest(req AttestationRequest) error {
	if len(req.PublicKey) == 0 {
		return fmt.Errorf("missing_public_key")
	}
	if len(req.PublicKey) > nsmFieldMaxSize {
		return fmt.Errorf("public_key_too_large")
	}
	if len(req.Nonce) > nsmFieldMaxSize {
		return fmt.Errorf("nonce_too_large")
	}
	if len(req.UserData) > nsmFieldMaxSize {
		return fmt.Errorf("user_data_too_large")
	}
	return nil
}

func encodeAttestationRequest(req AttestationRequest) ([]byte, error) {
	if err := validateAttestationRequest(req); err != nil {
		return nil, err
	}

	// {"Attestation": {"user_data": <bytes|null>, "nonce": <bytes|null>, "public_key": <bytes>}}
	inner := map[string]interface{}{
		"user_data":  nil,
		"nonce":      nil,
		"public_key": req.PublicKey,
	}
	if req.UserData != nil {
		inner["user_data"] = req.UserData
	}
	if req.Nonce != nil {
		inner["nonce"] = req.Nonce
	}
	outer := map[string]interface{}{
		"Attestation": inner,
	}
	return cbor.Marshal(outer)
}

func encodeGetRandomRequest() ([]byte, error) {
	// Unit variant: just the string "GetRandom"
	return cbor.Marshal("GetRandom")
}

func decodeAttestationResponse(data []byte) ([]byte, error) {
	resp, err := decodeNsmResponse(data)
	if err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("nsm error: %s", resp.Error)
	}
	if resp.Variant != "Attestation" {
		return nil, fmt.Errorf("unexpected response variant: %s", resp.Variant)
	}
	return resp.Document, nil
}

func decodeGetRandomResponse(data []byte) ([]byte, error) {
	resp, err := decodeNsmResponse(data)
	if err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("nsm error: %s", resp.Error)
	}
	if resp.Variant != "GetRandom" {
		return nil, fmt.Errorf("unexpected response variant: %s", resp.Variant)
	}
	return resp.Random, nil
}

type nsmResponse struct {
	Variant  string
	Document []byte // Attestation response
	Random   []byte // GetRandom response
	Error    string // Error response
}

func decodeNsmResponse(data []byte) (*nsmResponse, error) {
	// Try as map first (struct variants and Error)
	var m map[string]cbor.RawMessage
	if err := cbor.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("unknown response shape")
	}
	if len(m) != 1 {
		return nil, fmt.Errorf("ambiguous response shape")
	}

	for key, raw := range m {
		switch key {
		case "Attestation":
			var inner map[string]cbor.RawMessage
			if err := cbor.Unmarshal([]byte(raw), &inner); err != nil {
				return nil, fmt.Errorf("decode attestation: %w", err)
			}
			rawDocument, ok := inner["document"]
			if !ok {
				return nil, fmt.Errorf("decode attestation: missing document")
			}
			var document []byte
			if err := cbor.Unmarshal([]byte(rawDocument), &document); err != nil {
				return nil, fmt.Errorf("decode attestation document: %w", err)
			}
			if len(document) == 0 {
				return nil, fmt.Errorf("decode attestation: empty document")
			}
			return &nsmResponse{Variant: "Attestation", Document: document}, nil

		case "GetRandom":
			var inner map[string]cbor.RawMessage
			if err := cbor.Unmarshal([]byte(raw), &inner); err != nil {
				return nil, fmt.Errorf("decode random: %w", err)
			}
			rawRandom, ok := inner["random"]
			if !ok {
				return nil, fmt.Errorf("decode random: missing random")
			}
			var random []byte
			if err := cbor.Unmarshal([]byte(rawRandom), &random); err != nil {
				return nil, fmt.Errorf("decode random bytes: %w", err)
			}
			if len(random) == 0 {
				return nil, fmt.Errorf("decode random: empty random")
			}
			return &nsmResponse{Variant: "GetRandom", Random: random}, nil

		case "Error":
			var errCode string
			if err := cbor.Unmarshal([]byte(raw), &errCode); err != nil {
				return nil, fmt.Errorf("decode error: %w", err)
			}
			if errCode == "" {
				return nil, fmt.Errorf("decode error: empty error code")
			}
			return &nsmResponse{Variant: "Error", Error: errCode}, nil
		}
	}

	return nil, fmt.Errorf("unknown response shape")
}

// --- Line protocol handler ---

func handleNsmLine(backend NsmBackend, line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "0 ERR empty_request"
	}
	if strings.HasPrefix(trimmed, "{") {
		return handleNsmJSONLine(backend, trimmed)
	}

	return handleNsmLegacyLine(backend, trimmed)
}

type nsmJSONRequest struct {
	ID        string `json:"id"`
	Method    string `json:"method"`
	PublicKey string `json:"publicKey,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	UserData  string `json:"userData,omitempty"`
}

type nsmJSONResponse struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func handleNsmJSONLine(backend NsmBackend, line string) string {
	var req nsmJSONRequest
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return marshalNsmJSONResponse(nsmJSONResponse{OK: false, Error: "invalid_json"})
	}
	if err := ensureNoTrailingJSON(dec); err != nil {
		return marshalNsmJSONResponse(nsmJSONResponse{OK: false, Error: "invalid_json"})
	}
	id := req.ID
	if !validNsmID(id) {
		return marshalNsmJSONResponse(nsmJSONResponse{OK: false, Error: "invalid_id"})
	}

	switch req.Method {
	case "ATT":
		publicKey, errCode := decodeRequiredHex(req.PublicKey, "missing_public_key")
		if errCode != "" {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: errCode})
		}
		nonce, errCode := decodeOptionalHex(req.Nonce)
		if errCode != "" {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: errCode})
		}
		userData, errCode := decodeOptionalHex(req.UserData)
		if errCode != "" {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: errCode})
		}
		doc, err := backend.GetAttestation(AttestationRequest{PublicKey: publicKey, Nonce: nonce, UserData: userData})
		if err != nil {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: protocolError(err.Error())})
		}
		return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: true, Data: hex.EncodeToString(doc)})

	case "RND":
		if req.PublicKey != "" || req.Nonce != "" || req.UserData != "" {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: "invalid_request"})
		}
		random, err := backend.GetRandom()
		if err != nil {
			return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: protocolError(err.Error())})
		}
		return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: true, Data: hex.EncodeToString(random)})

	default:
		return marshalNsmJSONResponse(nsmJSONResponse{ID: id, OK: false, Error: "unknown_method"})
	}
}

func marshalNsmJSONResponse(resp nsmJSONResponse) string {
	data, err := json.Marshal(resp)
	if err != nil {
		return `{"id":"","ok":false,"error":"internal_error"}`
	}
	return string(data)
}

func handleNsmLegacyLine(backend NsmBackend, trimmed string) string {
	parts := strings.Fields(trimmed)
	id := "0"
	if len(parts) > 0 {
		id = parts[0]
	}
	if !validNsmID(id) {
		return "0 ERR invalid_id"
	}

	if len(parts) < 2 {
		return fmt.Sprintf("%s ERR invalid_request", id)
	}

	method := parts[1]

	switch method {
	case "ATT":
		if len(parts) == 2 {
			return fmt.Sprintf("%s ERR missing_public_key", id)
		}
		if len(parts) != 3 && len(parts) != 5 {
			return fmt.Sprintf("%s ERR invalid_request", id)
		}
		publicKey, errCode := decodeRequiredHex(parts[2], "missing_public_key")
		if errCode != "" {
			return fmt.Sprintf("%s ERR %s", id, errCode)
		}
		var nonce []byte
		var userData []byte
		if len(parts) == 5 {
			nonce, errCode = decodeOptionalLegacyHex(parts[3])
			if errCode != "" {
				return fmt.Sprintf("%s ERR %s", id, errCode)
			}
			userData, errCode = decodeOptionalLegacyHex(parts[4])
			if errCode != "" {
				return fmt.Sprintf("%s ERR %s", id, errCode)
			}
		}
		doc, err := backend.GetAttestation(AttestationRequest{PublicKey: publicKey, Nonce: nonce, UserData: userData})
		if err != nil {
			return fmt.Sprintf("%s ERR %s", id, protocolError(err.Error()))
		}
		return fmt.Sprintf("%s OK %s", id, hex.EncodeToString(doc))

	case "RND":
		if len(parts) != 2 {
			return fmt.Sprintf("%s ERR invalid_request", id)
		}
		random, err := backend.GetRandom()
		if err != nil {
			return fmt.Sprintf("%s ERR %s", id, protocolError(err.Error()))
		}
		return fmt.Sprintf("%s OK %s", id, hex.EncodeToString(random))

	default:
		return fmt.Sprintf("%s ERR unknown_method", id)
	}
}

func decodeRequiredHex(s, missingCode string) ([]byte, string) {
	if s == "" {
		return nil, missingCode
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return nil, "invalid_hex"
	}
	if len(decoded) == 0 {
		return nil, missingCode
	}
	return decoded, ""
}

func decodeOptionalHex(s string) ([]byte, string) {
	if s == "" {
		return nil, ""
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return nil, "invalid_hex"
	}
	return decoded, ""
}

func decodeOptionalLegacyHex(s string) ([]byte, string) {
	if s == "-" {
		return nil, ""
	}
	return decodeOptionalHex(s)
}

func validNsmID(id string) bool {
	if id == "" || len(id) > nsmIDMaxSize {
		return false
	}
	for _, b := range []byte(id) {
		switch {
		case b >= 'a' && b <= 'z':
		case b >= 'A' && b <= 'Z':
		case b >= '0' && b <= '9':
		case b == '.', b == '_', b == ':', b == '-':
		default:
			return false
		}
	}
	return true
}

func protocolError(reason string) string {
	if reason == "" {
		return "backend_error"
	}
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return "backend_error"
		}
	}
	return reason
}

// --- NSM subcommand entry point ---

func nsmMode() {
	backend, err := OpenNsm()
	if err != nil {
		log.Fatalf("[nsm] %v", err)
	}
	defer backend.Close()

	log.Println("[nsm] ready")

	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	for {
		line, err := readNsmLine(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooLarge *nsmLineTooLargeError
			if errors.As(err, &tooLarge) {
				if tooLarge.jsonLike {
					fmt.Fprintln(writer, marshalNsmJSONResponse(nsmJSONResponse{OK: false, Error: "line_too_large"}))
				} else {
					fmt.Fprintln(writer, "0 ERR line_too_large")
				}
				writer.Flush()
				continue
			}
			log.Printf("[nsm] stdin error: %v", err)
			break
		}
		response := handleNsmLine(backend, line)
		fmt.Fprintln(writer, response)
		writer.Flush()
	}
}

type nsmLineTooLargeError struct {
	jsonLike bool
}

func (e *nsmLineTooLargeError) Error() string {
	return "line_too_large"
}

func readNsmLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > nsmLineMaxSize {
			jsonLike := nsmLineLooksJSON(line, fragment)
			discardNsmLineRemainder(reader, err)
			return "", &nsmLineTooLargeError{jsonLike: jsonLike}
		}
		line = append(line, fragment...)

		switch err {
		case nil:
			return strings.TrimRight(string(line), "\r\n"), nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return "", io.EOF
			}
			return string(line), nil
		default:
			return "", err
		}
	}
}

func discardNsmLineRemainder(reader *bufio.Reader, err error) {
	for err == bufio.ErrBufferFull {
		_, err = reader.ReadSlice('\n')
	}
}

func nsmLineLooksJSON(parts ...[]byte) bool {
	for _, part := range parts {
		for _, b := range part {
			switch b {
			case ' ', '\t', '\r', '\n':
				continue
			default:
				return b == '{'
			}
		}
	}
	return false
}
