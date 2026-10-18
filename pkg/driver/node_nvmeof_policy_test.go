package driver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testNVMeHostnameProfile = `{"version":1,"profiles":[{"name":"nas","transport":"tcp","server":"nas.example.test","port":4420,"addresses":["192.0.2.10","2001:db8::10"],"parameters":{"nvmeof.ctrl-loss-tmo":"1800","nvmeof.max-sectors-kb":"1024","nvmeof.read-ahead-kb":"128","nvmeof.nr-io-queues":"8"}}]}`

func TestNVMePolicyDefaultsPreserveConnectArguments(t *testing.T) {
	s := NewNodeService("test", nil, true, nil, false, 1)
	params, err := s.validateNVMeOFParams(map[string]string{"server": "nas.example.test", "nqn": nqnA})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"connect", "-t", "tcp", "-n", nqnA, "-a", "nas.example.test", "-s", "4420", "--reconnect-delay=2", "--ctrl-loss-tmo=60", "--keep-alive-tmo=5", "--nr-io-queues=4"}
	if got := nvmeConnectArguments(params); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments=%v; want %v", got, want)
	}
	if len(params.explicitPolicy.parameters()) != 0 || len(params.addressPins) != 0 {
		t.Fatal("omitted policy acquired explicit values or endpoint pins")
	}
}

func TestNVMePolicyRejectsInvalidNewValues(t *testing.T) {
	for _, key := range []string{nvmeReconnectDelayKey, nvmeCtrlLossKey, nvmeKeepAliveKey, nvmeMaxSectorsKey, nvmeReadAheadKey} {
		for _, value := range []string{"", "-1", "+1", " 1", "1 ", "1s", "1.5", "0x10", "18446744073709551616", "private-test-value"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				_, err := parseNVMePolicy(map[string]string{key: value}, false)
				if !errors.Is(err, errInvalidNVMePolicy) || strings.Contains(err.Error(), "private-test-value") {
					t.Fatalf("policy error=%v; want a bounded error without input value", err)
				}
			})
		}
	}
	if _, err := parseNVMePolicy(map[string]string{nvmeCtrlLossKey: "0"}, false); err == nil {
		t.Fatal("zero controller-loss timeout accepted")
	}
	policy, err := parseNVMePolicy(map[string]string{nvmeReadAheadKey: "0"}, false)
	if err != nil || policy.parameters()[nvmeReadAheadKey] != "0" {
		t.Fatalf("explicit zero readahead lost: %v, %v", policy.parameters(), err)
	}
}

func TestNVMePolicyKeepsLegacyQueueInterpretation(t *testing.T) {
	for _, value := range []string{"010", "0x10", "+8", "16legacy-suffix"} {
		policy, err := parseNVMePolicy(map[string]string{nvmeIOQueuesKey: value, "unrelated": "ignored"}, false)
		if err != nil || policy.parameters()[nvmeIOQueuesKey] != value {
			t.Fatalf("legacy value %q changed: %v, %v", value, policy.parameters(), err)
		}
	}
	policy, err := parseNVMePolicy(map[string]string{nvmeQueueSizeKey: ""}, false)
	if err != nil || len(policy.parameters()) != 0 {
		t.Fatalf("empty legacy queue size became explicit: %v, %v", policy.parameters(), err)
	}
}

func TestNVMeHostnameProfilePinsEndpointAndPreservesPresence(t *testing.T) {
	profile, err := parseNVMeNodePolicy([]byte(testNVMeHostnameProfile))
	if err != nil {
		t.Fatal(err)
	}
	s := NewNodeService("test", nil, true, nil, false, 1, WithNVMeNodePolicy(profile))
	params, err := s.validateNVMeOFParams(map[string]string{"server": "NAS.EXAMPLE.TEST.", "nqn": nqnA, nvmeReadAheadKey: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if params.connectServer != "192.0.2.10" || len(params.addressPins) != 2 || params.policy.values[nvmeReadAheadKey] != 0 || params.policy.values[nvmeCtrlLossKey] != 1800 {
		t.Fatalf("resolved policy=%+v, pins=%v", params.policy, params.addressPins)
	}
	if got := params.explicitPolicy.parameters(); !reflect.DeepEqual(got, map[string]string{nvmeReadAheadKey: "0"}) {
		t.Fatalf("profile values froze into explicit PV fields: %v", got)
	}
	args := strings.Join(nvmeConnectArguments(params), " ")
	if !strings.Contains(args, "-a 192.0.2.10") || !strings.Contains(args, "--ctrl-loss-tmo=1800") || !strings.Contains(args, "--nr-io-queues=8") {
		t.Fatalf("resolved arguments=%s", args)
	}
	other, err := s.validateNVMeOFParams(map[string]string{"server": "other.example.test", "nqn": nqnA})
	if err != nil || len(other.policy.parameters()) != 0 || other.connectServer != "" {
		t.Fatalf("unmatched target acquired policy: %+v, %v", other, err)
	}
}

func TestNVMeProfilesRejectAmbiguousConfiguration(t *testing.T) {
	invalid := []string{
		strings.Replace(testNVMeHostnameProfile, `"version":1`, `"version":2`, 1),
		strings.Replace(testNVMeHostnameProfile, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(testNVMeHostnameProfile, `"name":"nas"`, `"name":"nas","unexpected":true`, 1),
		strings.Replace(testNVMeHostnameProfile, `"addresses":["192.0.2.10","2001:db8::10"],`, "", 1),
		strings.Replace(testNVMeHostnameProfile, `"parameters":{`, `"nqns":[],"parameters":{`, 1),
		strings.Replace(testNVMeHostnameProfile, `"nvmeof.ctrl-loss-tmo":"1800"`, `"nvmeof.ctrl-loss-tmo":"1800","nvmeof.ctrl-loss-tmo":"900"`, 1),
		strings.Replace(testNVMeHostnameProfile, `"nvmeof.nr-io-queues":"8"`, `"nvmeof.nr-io-queues":"0x8"`, 1),
		testNVMeHostnameProfile + `{}`,
	}
	for index, config := range invalid {
		if _, err := parseNVMeNodePolicy([]byte(config)); err == nil {
			t.Fatalf("invalid profile case %d accepted", index)
		}
	}
}

func TestNVMeProfileStartupFailsBeforeNASClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewDriver(Config{NVMeNodeProfilePath: path, APIURL: "invalid-url", APIKey: "generic-test-key"})
	if !errors.Is(err, errInvalidNVMePolicy) {
		t.Fatalf("startup reached client creation before profile validation: %v", err)
	}
	profile, err := LoadNVMeNodePolicy("")
	if err != nil || len(profile.profiles) != 0 {
		t.Fatalf("empty profile path=%v, %v", profile, err)
	}
}

func TestNVMeExplicitHostnamePolicyRequiresEndpointProof(t *testing.T) {
	s := NewNodeService("test", nil, true, nil, false, 1)
	_, err := s.validateNVMeOFParams(map[string]string{"server": "nas.example.test", "nqn": nqnA, nvmeCtrlLossKey: "1800"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unbound hostname policy=%v; want FailedPrecondition", err)
	}
}

func TestNVMeProfileExactSelectorsAndOverlap(t *testing.T) {
	config := nvmePolicyConfig{Version: 1, Profiles: []nvmePolicyProfileConfig{
		{Name: "cap", Transport: "tcp", Server: "[2001:db8::10]", Port: 4421, NQNs: &[]string{nqnA}, Parameters: map[string]string{nvmeMaxSectorsKey: "1024"}},
		{Name: "ahead", Transport: "tcp", Server: "2001:db8::10", Port: 4421, NQNs: &[]string{nqnA}, Parameters: map[string]string{nvmeReadAheadKey: "0"}},
	}}
	encode := func() []byte {
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	profile, err := parseNVMeNodePolicy(encode())
	if err != nil {
		t.Fatal(err)
	}
	s := NewNodeService("test", nil, true, nil, false, 1, WithNVMeNodePolicy(profile))
	for _, match := range []struct {
		server, port, nqn string
		wanted            int
	}{
		{"2001:0db8::10", "4421", nqnA, 2},
		{"2001:db8::10", "4420", nqnA, 0},
		{"2001:db8::11", "4421", nqnA, 0},
		{"2001:db8::10", "4421", nqnA + "-other", 0},
	} {
		params, parameterErr := s.validateNVMeOFParams(map[string]string{"server": match.server, "port": match.port, "nqn": match.nqn})
		if parameterErr != nil || len(params.policy.values) != match.wanted {
			t.Fatalf("selector %+v: %+v, %v", match, params, parameterErr)
		}
	}
	config.Profiles[1].Parameters[nvmeMaxSectorsKey] = "512"
	if _, conflictErr := parseNVMeNodePolicy(encode()); conflictErr == nil {
		t.Fatal("conflicting overlapping profiles accepted")
	}
	if _, bracketErr := canonicalNVMeServer("[nas.example.test]"); bracketErr == nil {
		t.Fatal("bracketed hostname accepted as IPv6")
	}
}
