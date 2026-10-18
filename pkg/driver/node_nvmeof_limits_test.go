package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/mount"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nvmeQueueWrite struct {
	path, key string
	value     uint64
}

func nvmeLimitFixture(t *testing.T, multipath bool) (*NodeService, map[string]string, *[]nvmeQueueWrite) {
	t.Helper()
	owners := map[string]string{"nvme7": nqnA, "nvme4": nqnB}
	devices := map[string]string{"nvme0n1": "nvme7", "nvme4n1": "nvme4"}
	if multipath {
		owners["nvme-subsys0"] = nqnA
		devices["nvme0n1"] = "nvme-subsys0"
		devices["nvme0c7n1"] = "nvme7"
	}
	block := fakeSysBlock(t, owners, devices, nil)
	originalBlock := sysBlockPath
	sysBlockPath = block
	t.Cleanup(func() { sysBlockPath = originalBlock })
	root := filepath.Dir(block)
	for name := range owners {
		owner := filepath.Join(root, "owners", name)
		if strings.HasPrefix(name, "nvme-subsys") {
			if err := os.Symlink(filepath.Join(root, "owners", "nvme7"), filepath.Join(owner, "nvme7")); err != nil {
				t.Fatal(err)
			}
			continue
		}
		for key, value := range map[string]string{"transport": "tcp", "address": "traddr=192.0.2.10,trsvcid=4420,host_traddr=192.0.2.20"} {
			writeNVMeFixtureValue(t, filepath.Join(owner, key), value)
		}
	}
	for name := range devices {
		path := filepath.Join(block, name)
		for key, value := range map[string]string{"dev": "259:0", "uuid": "11111111-1111-1111-1111-111111111111", "queue/max_sectors_kb": "4096", "queue/max_hw_sectors_kb": "2048", "queue/logical_block_size": "4096", "queue/read_ahead_kb": "128"} {
			writeNVMeFixtureValue(t, filepath.Join(path, key), value)
		}
		if nvmePathNamespaceName.MatchString(name) {
			if err := os.Remove(filepath.Join(path, "dev")); err != nil {
				t.Fatal(err)
			}
		}
	}
	service := NewNodeService("test", nil, false, nil, false, 1)
	service.nvmePolicyDirectory = filepath.Join(t.TempDir(), "records")
	service.nvmeDeviceNumberFn = func(string) (string, error) { return "259:0", nil }
	writes := &[]nvmeQueueWrite{}
	service.nvmeQueueWriteFn = func(ctx context.Context, path, key string, value uint64, verify func() error) error {
		if err := verify(); err != nil {
			return err
		}
		*writes = append(*writes, nvmeQueueWrite{path, key, value})
		writeNVMeFixtureValue(t, filepath.Join(path, "queue", key), strconv.FormatUint(value, 10))
		if key == "max_sectors_kb" {
			// Model a kernel cap update resetting readahead.
			writeNVMeFixtureValue(t, filepath.Join(path, "queue", "read_ahead_kb"), "256")
		}
		return ctx.Err()
	}
	return service, map[string]string{"server": "192.0.2.10", "nqn": nqnA, nvmeMaxSectorsKey: "1025", nvmeReadAheadKey: "0"}, writes
}

func writeNVMeFixtureValue(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNVMeQueueCapPrecedesReadAheadAndExcludesOtherNQN(t *testing.T) {
	s, parameters, writes := nvmeLimitFixture(t, true)
	params, parameterErr := s.validateNVMeOFParams(parameters)
	if parameterErr != nil {
		t.Fatal(parameterErr)
	}
	if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", filepath.Join(t.TempDir(), "stage"), params); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 4 {
		t.Fatalf("writes=%v; want head and path cap/readahead", *writes)
	}
	for index := 0; index < len(*writes); index += 2 {
		capWrite, aheadWrite := (*writes)[index], (*writes)[index+1]
		if capWrite.key != "max_sectors_kb" || capWrite.value != 1024 || aheadWrite.key != "read_ahead_kb" || aheadWrite.value != 0 || capWrite.path != aheadWrite.path || strings.HasSuffix(capWrite.path, "nvme4n1") {
			t.Fatalf("unexpected queue ordering or scope: %v", *writes)
		}
	}
	if current, _ := readNVMeQueueNumber(filepath.Join(sysBlockPath, "nvme4n1"), "max_sectors_kb"); current != 4096 {
		t.Fatal("another NQN was modified")
	}
	// A stricter current limit wins over the configured request.
	writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "queue", "max_sectors_kb"), "512")
	if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", "", params); err != nil {
		t.Fatal(err)
	}
	if current, _ := readNVMeQueueNumber(filepath.Join(sysBlockPath, "nvme0n1"), "max_sectors_kb"); current != 512 {
		t.Fatal("lower current cap was raised")
	}
}

func TestNVMeAlignedCapBounds(t *testing.T) {
	for _, test := range []struct {
		requested, current, hardware, logical, want uint64
	}{
		{1025, 4096, 2048, 4096, 1024},
		{1024, 128, 2048, 512, 128},
		{1024, 4096, 256, 4096, 256},
		{1, 4096, 4096, 4096, 0},
		{4096, 0, 4096, 4096, 0},
		{4096, 4096, 4096, 1000, 0},
	} {
		got, err := nvmeAlignedCap(test.requested, test.current, test.hardware, test.logical)
		if got != test.want || (err != nil) != (test.want == 0) {
			t.Fatalf("aligned cap %+v = %d, %v", test, got, err)
		}
	}
}

func TestNVMeQueueScopeRefusalsMakeNoWrites(t *testing.T) {
	for _, refusal := range []string{"nqn", "nsid", "transport", "address", "port", "uuid", "missing-dev", "stale-devnode", "missing-hardware", "extra-controller"} {
		t.Run(refusal, func(t *testing.T) {
			s, parameters, writes := nvmeLimitFixture(t, true)
			params, parameterErr := s.validateNVMeOFParams(parameters)
			if parameterErr != nil {
				t.Fatal(parameterErr)
			}
			controller := filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7")
			switch refusal {
			case "nqn":
				writeNVMeFixtureValue(t, filepath.Join(controller, "subsysnqn"), nqnB)
			case "nsid":
				writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "nsid"), "2")
			case "transport":
				writeNVMeFixtureValue(t, filepath.Join(controller, "transport"), "pcie")
			case "address":
				writeNVMeFixtureValue(t, filepath.Join(controller, "address"), "traddr=192.0.2.11,trsvcid=4420")
			case "port":
				writeNVMeFixtureValue(t, filepath.Join(controller, "address"), "traddr=192.0.2.10,trsvcid=4421")
			case "uuid":
				writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0c7n1", "uuid"), "22222222-2222-2222-2222-222222222222")
			case "missing-dev":
				if err := os.Remove(filepath.Join(sysBlockPath, "nvme0n1", "dev")); err != nil {
					t.Fatal(err)
				}
			case "stale-devnode":
				s.nvmeDeviceNumberFn = func(string) (string, error) { return "259:999", nil }
			case "missing-hardware":
				if err := os.Remove(filepath.Join(sysBlockPath, "nvme0c7n1", "queue", "max_hw_sectors_kb")); err != nil {
					t.Fatal(err)
				}
			case "extra-controller":
				if err := os.Symlink(filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme4"), filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme-subsys0", "nvme4")); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", "", params); err == nil || len(*writes) != 0 {
				t.Fatalf("refusal=%v writes=%v; want failure before mutation", err, *writes)
			}
		})
	}
}

func TestNVMeQueueRevalidationAndReadbackRefuse(t *testing.T) {
	for _, cause := range []string{"identity-swap", "ignored-write", "canceled"} {
		t.Run(cause, func(t *testing.T) {
			s, parameters, _ := nvmeLimitFixture(t, false)
			params, parameterErr := s.validateNVMeOFParams(parameters)
			if parameterErr != nil {
				t.Fatal(parameterErr)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := s.nvmeQueueWriteFn
			calls := 0
			s.nvmeQueueWriteFn = func(ctx context.Context, path, key string, value uint64, verify func() error) error {
				calls++
				switch cause {
				case "identity-swap":
					writeNVMeFixtureValue(t, filepath.Join(path, "uuid"), "22222222-2222-2222-2222-222222222222")
					return verify()
				case "canceled":
					cancel()
					return verify()
				default:
					if key == "read_ahead_kb" {
						return original(ctx, path, key, value, verify)
					}
					return nil
				}
			}
			if err := s.prepareNVMePolicy(ctx, "volume", "/dev/nvme0n1", "", params); err == nil {
				t.Fatal("unverified queue enforcement reported success")
			}
			if calls > 1 && cause != "ignored-write" {
				t.Fatalf("continued writes after refusal: %d", calls)
			}
		})
	}
}

func TestNVMeNodeRecordRestartAndCurrentProfile(t *testing.T) {
	s, parameters, _ := nvmeLimitFixture(t, false)
	profile, err := parseNVMeNodePolicy([]byte(testNVMeHostnameProfile))
	if err != nil {
		t.Fatal(err)
	}
	s.nvmeNodePolicy = profile
	parameters["server"] = "nas.example.test"
	delete(parameters, nvmeMaxSectorsKey)
	params, parameterErr := s.validateNVMeOFParams(parameters)
	if parameterErr != nil {
		t.Fatal(parameterErr)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if prepareErr := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", stage, params); prepareErr != nil {
		t.Fatal(prepareErr)
	}
	record, err := s.readNVMeNodeRecord("volume", stage)
	if err != nil || !reflect.DeepEqual(record.Target.Parameters, map[string]string{nvmeReadAheadKey: "0"}) {
		t.Fatalf("explicit provenance=%+v, %v", record, err)
	}
	updated, err := parseNVMeNodePolicy([]byte(strings.Replace(testNVMeHostnameProfile, `"nvmeof.max-sectors-kb":"1024"`, `"nvmeof.max-sectors-kb":"256"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewNodeService("test", nil, false, nil, false, 1, WithNVMeNodePolicy(updated))
	restarted.nvmePolicyDirectory = s.nvmePolicyDirectory
	restarted.nvmeQueueWriteFn = s.nvmeQueueWriteFn
	restarted.nvmeDeviceNumberFn = s.nvmeDeviceNumberFn
	restored, err := restarted.restoreNVMeNodeParams(context.Background(), "volume", stage)
	if err != nil || restored.policy.values[nvmeMaxSectorsKey] != 256 || restored.policy.values[nvmeReadAheadKey] != 0 {
		t.Fatalf("restored policy=%+v, %v", restored, err)
	}
	if err := restarted.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", stage, restored); err != nil {
		t.Fatal(err)
	}
	capKiB, _ := readNVMeQueueNumber(filepath.Join(sysBlockPath, "nvme0n1"), "max_sectors_kb")
	if capKiB != 256 {
		t.Fatalf("current profile was not applied: %d", capKiB)
	}
	if info, err := os.Stat(filepath.Join(s.nvmePolicyDirectory, nvmeRecordName("volume"))); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("policy record permissions=%v, %v", info, err)
	}
}

func TestNVMeNodeRecordRefusesLostOrChangedEvidence(t *testing.T) {
	for _, cause := range []string{"missing", "corrupt", "version", "changed-volume", "changed-namespace", "omitted-cap", "wrong-marker"} {
		t.Run(cause, func(t *testing.T) {
			s, parameters, writes := nvmeLimitFixture(t, false)
			params, parameterErr := s.validateNVMeOFParams(parameters)
			if parameterErr != nil {
				t.Fatal(parameterErr)
			}
			stage := filepath.Join(t.TempDir(), "stage")
			if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", stage, params); err != nil {
				t.Fatal(err)
			}
			*writes = nil
			path := filepath.Join(s.nvmePolicyDirectory, nvmeRecordName("volume"))
			switch cause {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				writeNVMeFixtureValue(t, path, "{")
			case "version", "changed-volume":
				record, err := s.readNVMeNodeRecord("volume", stage)
				if err != nil {
					t.Fatal(err)
				}
				if cause == "version" {
					record.Version = 99
				} else {
					record.Target.VolumeID = "other"
				}
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				writeNVMeFixtureValue(t, path, string(data))
			case "changed-namespace":
				writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "uuid"), "22222222-2222-2222-2222-222222222222")
			case "omitted-cap":
				delete(parameters, nvmeMaxSectorsKey)
				params, parameterErr = s.validateNVMeOFParams(parameters)
				if parameterErr != nil {
					t.Fatal(parameterErr)
				}
			case "wrong-marker":
				writeNVMeFixtureValue(t, stage+nvmePolicyMarkerSuffix, nvmeRecordName("other"))
			}
			if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", stage, params); err == nil || len(*writes) != 0 {
				t.Fatalf("unsafe retained policy accepted: %v, writes=%v", err, *writes)
			}
		})
	}
}

func TestNVMePublicUseRefusesPolicyFailure(t *testing.T) {
	s, parameters, writes := nvmeLimitFixture(t, false)
	parameters["protocol"] = ProtocolNVMeOF
	writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "queue", "max_hw_sectors_kb"), "0")
	originalLookup := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "/dev/nvme0n1", FSType: "ext4"}, true, nil
	}
	t.Cleanup(func() { lookupMountedEntry = originalLookup })
	stage := filepath.Join(t.TempDir(), "stage")
	block := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}}
	_, err := s.stageNVMeDevice(context.Background(), "volume", "/dev/nvme0n1", stage, block, true, parameters)
	if status.Code(err) != codes.Unavailable || len(*writes) != 0 {
		t.Fatalf("stage bypassed cap failure: %v, writes=%v", err, *writes)
	}
	_, err = s.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "volume", StagingTargetPath: stage, TargetPath: filepath.Join(t.TempDir(), "publish"), VolumeCapability: block, VolumeContext: parameters})
	if status.Code(err) != codes.Unavailable || len(*writes) != 0 {
		t.Fatalf("publish bypassed cap failure: %v, writes=%v", err, *writes)
	}
}

func TestNVMeRawExpandRestoresCapBeforeNoResizeSuccess(t *testing.T) {
	s, parameters, writes := nvmeLimitFixture(t, false)
	params, parameterErr := s.validateNVMeOFParams(parameters)
	if parameterErr != nil {
		t.Fatal(parameterErr)
	}
	if err := s.prepareNVMePolicy(context.Background(), "volume", "/dev/nvme0n1", "", params); err != nil {
		t.Fatal(err)
	}
	*writes = nil
	s.nvmeExpandDeviceFn = func(string) (string, error) { return "/dev/nvme0n1", nil }
	writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "queue", "max_sectors_kb"), "4096")
	path := filepath.Join(t.TempDir(), "published")
	writeNVMeFixtureValue(t, path, "fixture")
	request := &csi.NodeExpandVolumeRequest{VolumeId: "volume", VolumePath: path, VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}}, CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30}}
	response, err := s.NodeExpandVolume(context.Background(), request)
	if err != nil || response.CapacityBytes != 2<<30 || len(*writes) != 2 {
		t.Fatalf("raw expansion=%v, %v; writes=%v", response, err, *writes)
	}
	writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "queue", "max_hw_sectors_kb"), "0")
	if _, err := s.NodeExpandVolume(context.Background(), request); err == nil {
		t.Fatal("raw no-resize path ignored cap failure")
	}
}

func TestNVMePolicyFileAndLegacyAbsence(t *testing.T) {
	s, _, writes := nvmeLimitFixture(t, false)
	if params, err := s.restoreNVMeNodeParams(context.Background(), "legacy", ""); !errors.Is(err, errNVMePolicyRecordNotFound) || params != nil || len(*writes) != 0 {
		t.Fatalf("legacy absence=%v, %v", params, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "policy")
	if err := writeNVMePolicyFile(ctx, path, []byte("value")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled persistence=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled call persisted metadata")
	}
	for _, address := range []string{"traddr=192.0.2.10,trsvcid=4420,traddr=192.0.2.10", "traddr=nas.example.test,trsvcid=4420", "invalid"} {
		params, err := s.validateNVMeOFParams(map[string]string{"nqn": nqnA, "server": "192.0.2.10", nvmeReadAheadKey: "0"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nvmeVerifiedEndpoint(address, params); err == nil {
			t.Fatalf("ambiguous endpoint accepted: %s", address)
		}
	}
	if got := fmt.Sprint(*writes); got != "[]" {
		t.Fatalf("legacy or invalid endpoint wrote queues: %s", got)
	}
}

func TestNVMePinnedQueueWriter(t *testing.T) {
	s, parameters, _ := nvmeLimitFixture(t, false)
	delete(parameters, nvmeReadAheadKey)
	parameters[nvmeMaxSectorsKey] = "1024"
	params, err := s.validateNVMeOFParams(parameters)
	if err != nil {
		t.Fatal(err)
	}
	s.nvmeQueueWriteFn = nil
	if _, enforcementErr := s.enforceNVMeQueuePolicy(context.Background(), "/dev/nvme0n1", params); enforcementErr != nil {
		t.Fatal(enforcementErr)
	}
	if value, readErr := readNVMeQueueNumber(filepath.Join(sysBlockPath, "nvme0n1"), "max_sectors_kb"); readErr != nil || value != 1024 {
		t.Fatalf("pinned writer result=%d, %v", value, readErr)
	}
}

func TestNVMeExpansionCannotMistakeLostRecordForLegacyPolicy(t *testing.T) {
	for _, cause := range []string{"retained-remote-policy", "api-unavailable", "profile-needs-binding", "legacy"} {
		t.Run(cause, func(t *testing.T) {
			s, _, writes := nvmeLimitFixture(t, false)
			mock := &MockAPIClientForSnapshots{}
			mock.GetDatasetPropertiesFunc = func(context.Context, string, []string) (map[string]string, error) {
				if cause == "api-unavailable" {
					return nil, errors.New("metadata API unavailable")
				}
				if cause == "retained-remote-policy" {
					return map[string]string{tnsapi.PropertyNVMePolicy: "retained-data"}, nil
				}
				return map[string]string{}, nil
			}
			s.apiClient = mock
			if cause == "profile-needs-binding" {
				profile, err := parseNVMeNodePolicy([]byte(testNVMeHostnameProfile))
				if err != nil {
					t.Fatal(err)
				}
				s.nvmeNodePolicy = profile
			}
			_, err := s.restoreNVMeNodeParams(context.Background(), "volume", "")
			want := codes.FailedPrecondition
			if cause == "api-unavailable" {
				want = codes.Unavailable
			}
			if cause == "legacy" {
				if !errors.Is(err, errNVMePolicyRecordNotFound) {
					t.Fatalf("known legacy policy absence=%v", err)
				}
			} else if status.Code(err) != want {
				t.Fatalf("lost record resolution=%v; want %v", err, want)
			}
			if len(*writes) != 0 {
				t.Fatal("missing policy evidence modified a queue")
			}
		})
	}
}
