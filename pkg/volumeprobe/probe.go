// Package volumeprobe isolates read-only filesystem observations from CSI RPCs.
package volumeprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// HelperArgument selects the private helper mode before driver initialization.
	HelperArgument = "__volume-probe"
	defaultTimeout = 3 * time.Second
	maxOutput      = 8192
	maxRequest     = 32 * 1024
	// OK means the requested observations completed, not that writes or ownership were proven.
	OK = "ok"
	// IOFailure means a read-only filesystem operation returned an I/O failure.
	IOFailure = "io_error"
	// Unsupported means the platform or installed probe capability is unavailable.
	Unsupported = "unsupported"
	// Unknown means available evidence cannot establish liveness.
	Unknown = "unknown"
	// IdentityChanged means the opened filesystem differs from the requested mount.
	IdentityChanged = "identity_changed"
	// Missing means the requested path no longer exists.
	Missing = "missing"
	// TransportUnavailable means a known block transport is not running.
	TransportUnavailable = "transport_unavailable"
)

var (
	// ErrBusy means another probe still owns the volume or global slot.
	ErrBusy = errors.New("filesystem probe capacity is occupied")
	// ErrProtocol means a helper did not return a valid bounded response.
	ErrProtocol = errors.New("invalid filesystem probe response")
)

// Request contains mount-table identity; no application data is written.
type Request struct {
	Path         string `json:"path"`
	MetadataPath string `json:"metadataPath,omitempty"`
	MarkerPath   string `json:"markerPath,omitempty"`
	Device       string `json:"device"`
	Filesystem   string `json:"filesystem"`
	MountID      int    `json:"mountID"`
	ResolveMount bool   `json:"resolveMount,omitempty"`
	CheckSCSI    bool   `json:"checkSCSI,omitempty"`
}

// ResolvedMount identifies the visible mount after bounded path resolution.
type ResolvedMount struct {
	Path       string `json:"path"`
	Source     string `json:"source"`
	Device     string `json:"device"`
	Filesystem string `json:"filesystem"`
	MountID    int    `json:"mountID"`
	Raw        bool   `json:"raw,omitempty"`
}

// Result reports observed counters and XFS root-inode sickness, when available.
type Result struct {
	Mount          *ResolvedMount `json:"mount,omitempty"`
	Code           string         `json:"code"`
	RawDevice      string         `json:"rawDevice,omitempty"`
	StagingNQN     string         `json:"stagingNQN,omitempty"`
	PolicyRecord   string         `json:"policyRecord,omitempty"`
	TotalBytes     uint64         `json:"totalBytes"`
	FreeBytes      uint64         `json:"freeBytes"`
	AvailableBytes uint64         `json:"availableBytes"`
	TotalInodes    uint64         `json:"totalInodes"`
	FreeInodes     uint64         `json:"freeInodes"`
	RootSick       uint64         `json:"rootSick"`
	RootChecked    uint64         `json:"rootChecked"`
	Version        int            `json:"version"`
}

// RunHelper handles one private request. The caller must supply inherited lease FDs.
func RunHelper(ctx context.Context, input io.Reader, output io.Writer) int {
	request, err := decodeRequest(input)
	if err != nil {
		return 2
	}
	leases := []*os.File{os.NewFile(3, "global-probe-lease"), os.NewFile(4, "volume-probe-lease")}
	for _, lease := range leases {
		if lease == nil {
			return 2
		}
		if info, statErr := lease.Stat(); statErr != nil || !info.Mode().IsRegular() {
			return 2
		}
	}
	result := observe(ctx, request, leases)
	result.Version = 1
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return 2
	}
	// Leases remain referenced until all probing is finished, including nested tools.
	for _, lease := range leases {
		if err := lease.Close(); err != nil {
			return 2
		}
	}
	return 0
}

func decodeRequest(input io.Reader) (Request, error) {
	var request Request
	data, err := io.ReadAll(io.LimitReader(input, maxRequest+1))
	if err != nil || len(data) > maxRequest {
		return request, ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, ErrProtocol
	}
	if !filepath.IsAbs(request.Path) || (!request.ResolveMount && (request.MountID <= 0 || request.Device == "" || request.Filesystem == "")) {
		return request, ErrProtocol
	}
	for _, path := range []string{request.MetadataPath, request.MarkerPath} {
		if path != "" && !filepath.IsAbs(path) {
			return request, ErrProtocol
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return request, ErrProtocol
	}
	return request, nil
}

type limitedOutput struct {
	data     []byte
	mu       sync.Mutex
	overflow bool
}

func (b *limitedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxOutput - len(b.data)
	if len(data) > remaining {
		b.overflow = true
	}
	b.data = append(b.data, data[:min(len(data), remaining)]...)
	return len(data), nil
}

func (b *limitedOutput) contents() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data), b.overflow
}
