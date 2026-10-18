package performance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const (
	// SchemaVersion identifies the finite-file request format.
	SchemaVersion = 1
	// FioVersion is the output format reviewed against upstream stat.c.
	FioVersion = "fio-3.39"
)

// ErrInvalidManifest never embeds untrusted input or credential-bearing values.
var (
	ErrInvalidManifest = errors.New("invalid performance manifest")
	// ErrIdentityMismatch refuses incomplete or changed observed identities.
	ErrIdentityMismatch = errors.New("performance identity mismatch")
)

var (
	identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	scenario   = regexp.MustCompile(`^[BILFHAQUC]\d{2}$`)
	nqn        = regexp.MustCompile(`^nqn\.\d{4}-\d{2}\.[a-zA-Z0-9.-]+:[a-zA-Z0-9.:-]+$`)
)

// Manifest describes one finite read or write phase; it grants no runtime access.
type Manifest struct {
	Identity      Identity  `json:"identity"`
	RunID         string    `json:"run_id"`
	ScenarioID    string    `json:"scenario_id"`
	FioVersion    string    `json:"fio_version"`
	RunnerImage   string    `json:"runner_image"`
	DatasetParent string    `json:"dataset_parent"`
	Phase         string    `json:"phase"`
	Jobs          []JobPlan `json:"jobs"`
	Budget        Budget    `json:"budget"`
	SchemaVersion uint64    `json:"schema_version"`
}

// Identity contains expected immutable identifiers, not observations of ownership.
type Identity struct {
	PVCUID         string `json:"pvc_uid"`
	PVUID          string `json:"pv_uid"`
	ClaimNamespace string `json:"claim_namespace"`
	ClaimName      string `json:"claim_name"`
	ClaimRefUID    string `json:"claim_ref_uid"`
	VolumeHandle   string `json:"volume_handle"`
	NodeUID        string `json:"node_uid"`
	Dataset        string `json:"dataset"`
	NQN            string `json:"nqn"`
	NamespaceUUID  string `json:"namespace_uuid"`
	FilesystemUUID string `json:"filesystem_uuid"`
	FilesystemType string `json:"filesystem_type"`
	MountPath      string `json:"mount_path"`
	NSID           uint32 `json:"nsid"`
}

// Budget bounds file allocation and aggregate in-flight I/O buffers in bytes.
type Budget struct {
	CapacityBytes  uint64 `json:"capacity_bytes"`
	HeadroomBytes  uint64 `json:"headroom_bytes"`
	MaxMemoryBytes uint64 `json:"max_memory_bytes"`
}

// JobPlan assigns one unique file and a finite aligned extent to a named job.
type JobPlan struct {
	Name           string `json:"name"`
	Filename       string `json:"filename"`
	OffsetBytes    uint64 `json:"offset_bytes"`
	SizeBytes      uint64 `json:"size_bytes"`
	BlockSizeBytes uint64 `json:"block_size_bytes"`
	Depth          uint64 `json:"depth"`
}

// DecodeManifest accepts one strict JSON object without returning raw parse errors.
func DecodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := checkJSON(data, maxManifestBytes); err != nil {
		return manifest, ErrInvalidManifest
	}
	if err := checkShape(data, reflect.TypeFor[Manifest]()); err != nil {
		return manifest, ErrInvalidManifest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, ErrInvalidManifest
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate checks requested identities, file isolation, alignment and arithmetic.
// It cannot detect symlinks, stale mounts, other consumers or changes at runtime.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion || !identifier.MatchString(m.RunID) ||
		!validScenario(m.ScenarioID) || m.FioVersion != FioVersion ||
		!pinnedImage(m.RunnerImage) || (m.Phase != "read" && m.Phase != "write") {

		return ErrInvalidManifest
	}
	if !validIdentity(m.Identity, m.DatasetParent) || len(m.Jobs) == 0 || len(m.Jobs) > 1024 {
		return ErrInvalidManifest
	}
	if m.Budget.CapacityBytes == 0 || m.Budget.HeadroomBytes == 0 || m.Budget.MaxMemoryBytes == 0 {
		return ErrInvalidManifest
	}
	return m.validateJobs()
}

// CheckIdentity compares an independently observed chain with the request.
// A match does not establish observation freshness, visibility or writer exclusion.
func (m Manifest) CheckIdentity(observed Identity) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Identity != observed {
		return ErrIdentityMismatch
	}
	return nil
}

func validScenario(value string) bool {
	if !scenario.MatchString(value) {
		return false
	}
	limits := map[byte]int{'B': 7, 'I': 4, 'L': 6, 'F': 11, 'H': 8, 'A': 8, 'Q': 20, 'U': 4, 'C': 6}
	number, err := strconv.Atoi(value[1:])
	return err == nil && number > 0 && number <= limits[value[0]]
}

func (m Manifest) validateJobs() error {
	names := make(map[string]bool, len(m.Jobs))
	files := make(map[string]bool, len(m.Jobs))
	allocated, memory := m.Budget.HeadroomBytes, uint64(0)
	for _, job := range m.Jobs {
		if !validJob(job) || names[job.Name] || files[job.Filename] {
			return ErrInvalidManifest
		}
		names[job.Name], files[job.Filename] = true, true
		end, ok := add(job.OffsetBytes, job.SizeBytes)
		if !ok {
			return ErrInvalidManifest
		}
		allocated, ok = add(allocated, end)
		if !ok || allocated > m.Budget.CapacityBytes || job.Depth > math.MaxUint64/job.BlockSizeBytes {
			return ErrInvalidManifest
		}
		memory, ok = add(memory, job.Depth*job.BlockSizeBytes)
		if !ok || memory > m.Budget.MaxMemoryBytes {
			return ErrInvalidManifest
		}
	}
	return nil
}

func validJob(job JobPlan) bool {
	return identifier.MatchString(job.Name) && identifier.MatchString(job.Filename) &&
		job.BlockSizeBytes >= 512 && job.BlockSizeBytes%512 == 0 && job.SizeBytes > 0 &&
		job.SizeBytes%job.BlockSizeBytes == 0 && job.OffsetBytes%job.BlockSizeBytes == 0 && job.Depth > 0
}

func validIdentity(id Identity, parent string) bool {
	for _, value := range []string{id.PVCUID, id.PVUID, id.ClaimNamespace, id.ClaimName, id.NodeUID} {
		if !identifier.MatchString(value) {
			return false
		}
	}
	if id.ClaimRefUID != id.PVCUID || strings.TrimSpace(id.VolumeHandle) != id.VolumeHandle ||
		id.VolumeHandle == "" || len(id.VolumeHandle) > 512 || strings.ContainsAny(id.VolumeHandle, "\x00\r\n\t") {

		return false
	}
	return relativeDataset(parent) && relativeDataset(id.Dataset) &&
		strings.HasPrefix(id.Dataset, parent+"/") && len(id.NQN) <= 223 && nqn.MatchString(id.NQN) &&
		id.NSID > 0 && uuid(id.NamespaceUUID) && uuid(id.FilesystemUUID) &&
		(id.FilesystemType == "xfs" || id.FilesystemType == "ext4") && validMount(id.MountPath)
}

func relativeDataset(value string) bool {
	if value == "" || path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	for part := range strings.SplitSeq(value, "/") {
		if !identifier.MatchString(part) {
			return false
		}
	}
	return true
}

func validMount(value string) bool {
	if !path.IsAbs(value) || path.Clean(value) != value || value == "/" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, root := range []string{"/dev", "/proc", "/sys"} {
		if value == root || strings.HasPrefix(value, root+"/") {
			return false
		}
	}
	return true
}

func uuid(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16 && value != "00000000-0000-0000-0000-000000000000"
}

func pinnedImage(value string) bool {
	name, digest, ok := strings.Cut(value, "@sha256:")
	if !ok || name == "" || strings.ContainsAny(name, " \t\r\n\x00") || len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func add(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, false
	}
	return a + b, true
}
