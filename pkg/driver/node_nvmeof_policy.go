package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	nvmeTransportTCP      = "tcp"
	nvmeReconnectDelayKey = "nvmeof.reconnect-delay"
	nvmeCtrlLossKey       = "nvmeof.ctrl-loss-tmo"
	nvmeKeepAliveKey      = "nvmeof.keep-alive-tmo"
	nvmeMaxSectorsKey     = "nvmeof.max-sectors-kb"
	nvmeReadAheadKey      = "nvmeof.read-ahead-kb"
	nvmeIOQueuesKey       = "nvmeof.nr-io-queues"
	nvmeQueueSizeKey      = "nvmeof.queue-size"
	nvmePolicyFileLimit   = 1024 * 1024
)

var errInvalidNVMePolicy = errors.New("invalid NVMe policy")

func validateNVMePolicyParameters(parameters map[string]string) (nvmePolicy, error) {
	policy, err := parseNVMePolicy(parameters, false)
	if err != nil {
		return policy, err
	}
	if raw := parameters["port"]; raw != "" {
		port, parseErr := strconv.ParseUint(raw, 10, 16)
		if parseErr != nil || port == 0 || strconv.FormatUint(port, 10) != raw {
			return policy, fmt.Errorf("%w: invalid port", errInvalidNVMePolicy)
		}
	}
	if len(policy.values) > 0 && parameters["transport"] != "" && parameters["transport"] != nvmeTransportTCP {
		return policy, fmt.Errorf("%w: new policy requires TCP", errInvalidNVMePolicy)
	}
	return policy, nil
}

type nvmePolicy struct {
	values map[string]uint64
	legacy map[string]string
}

func parseNVMePolicy(parameters map[string]string, strict bool) (nvmePolicy, error) {
	policy := nvmePolicy{values: make(map[string]uint64), legacy: make(map[string]string)}
	for key, raw := range parameters {
		if key == nvmeIOQueuesKey || key == nvmeQueueSizeKey {
			if raw == "" && !strict {
				continue
			}
			// Preserve the existing CLI interpretation of legacy PV queue strings.
			if strict {
				value, err := parseNVMeDecimal(raw, math.MaxInt32, false)
				if err != nil || (key == nvmeQueueSizeKey && (value < 2 || value > 1024)) {
					return nvmePolicy{}, fmt.Errorf("%w: invalid %s", errInvalidNVMePolicy, key)
				}
				raw = strconv.FormatUint(value, 10)
			}
			policy.legacy[key] = raw
			continue
		}
		maximum := uint64(math.MaxInt32)
		switch key {
		case nvmeReconnectDelayKey, nvmeKeepAliveKey:
			// Keep milliseconds and supported HZ<=1000 timer products within int32.
			maximum = math.MaxInt32 / 1000
		case nvmeCtrlLossKey:
		case nvmeMaxSectorsKey, nvmeReadAheadKey:
			maximum = math.MaxInt64 / 1024
		default:
			if strict {
				return nvmePolicy{}, fmt.Errorf("%w: unknown parameter", errInvalidNVMePolicy)
			}
			continue
		}
		value, err := parseNVMeDecimal(raw, maximum, key == nvmeReadAheadKey)
		if err != nil {
			return nvmePolicy{}, fmt.Errorf("%w: invalid %s", errInvalidNVMePolicy, key)
		}
		policy.values[key] = value
	}
	return policy, nil
}

func parseNVMeDecimal(raw string, maximum uint64, allowZero bool) (uint64, error) {
	if raw == "" {
		return 0, errInvalidNVMePolicy
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, errInvalidNVMePolicy
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value > maximum || (value == 0 && !allowZero) {
		return 0, errInvalidNVMePolicy
	}
	return value, nil
}

func (p nvmePolicy) parameters() map[string]string {
	result := make(map[string]string, len(p.values)+len(p.legacy))
	for key, value := range p.values {
		result[key] = strconv.FormatUint(value, 10)
	}
	for key, value := range p.legacy {
		result[key] = value
	}
	return result
}

func (p nvmePolicy) argument(key, fallback string) string {
	if value, present := p.values[key]; present {
		return strconv.FormatUint(value, 10)
	}
	return fallback
}

func mergeNVMePolicies(explicit, fallback nvmePolicy) nvmePolicy {
	result := nvmePolicy{values: make(map[string]uint64), legacy: make(map[string]string)}
	for _, policy := range []nvmePolicy{fallback, explicit} {
		for key, value := range policy.values {
			result.values[key] = value
		}
		for key, value := range policy.legacy {
			result.legacy[key] = value
		}
	}
	return result
}

// NVMeNodePolicy contains immutable, validated target-scoped profiles.
type NVMeNodePolicy struct {
	profiles []nvmeNodeProfile
}

// NodeServiceOption adds optional node configuration without changing existing callers.
type NodeServiceOption func(*NodeService)

// WithNVMeNodePolicy enables validated target-scoped settings for the node.
func WithNVMeNodePolicy(policy *NVMeNodePolicy) NodeServiceOption {
	return func(service *NodeService) { service.nvmeNodePolicy = policy }
}

func loadNVMeDriverPolicy(cfg Config) (Config, error) {
	if cfg.NVMeNodePolicy == nil && cfg.NVMeNodeProfilePath != "" {
		policy, err := LoadNVMeNodePolicy(cfg.NVMeNodeProfilePath)
		if err != nil {
			return cfg, err
		}
		cfg.NVMeNodePolicy = policy
	}
	return cfg, nil
}

type nvmeNodeProfile struct {
	policy    nvmePolicy
	nqns      map[string]bool
	server    string
	addresses []netip.Addr
	port      uint16
}

type nvmePolicyConfig struct {
	Profiles []nvmePolicyProfileConfig `json:"profiles"`
	Version  int                       `json:"version"`
}

type nvmePolicyProfileConfig struct {
	Parameters map[string]string `json:"parameters"`
	NQNs       *[]string         `json:"nqns"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`
	Server     string            `json:"server"`
	Addresses  []string          `json:"addresses"`
	Port       uint16            `json:"port"`
}

// LoadNVMeNodePolicy reads settings only. An empty path performs no file I/O.
func LoadNVMeNodePolicy(path string) (*NVMeNodePolicy, error) {
	if path == "" {
		return &NVMeNodePolicy{}, nil
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, openErr := root.Open(filepath.Base(path))
	closeErr := root.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, nvmePolicyFileLimit+1))
	fileErr := file.Close()
	if err := errors.Join(readErr, fileErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > nvmePolicyFileLimit {
		return nil, fmt.Errorf("%w: profile file is too large", errInvalidNVMePolicy)
	}
	return parseNVMeNodePolicy(data)
}

func parseNVMeNodePolicy(data []byte) (*NVMeNodePolicy, error) {
	var config nvmePolicyConfig
	if err := decodeNVMePolicyJSON(data, &config); err != nil {
		return nil, err
	}
	if config.Version != 1 || len(config.Profiles) > 64 {
		return nil, fmt.Errorf("%w: unsupported version or profile count", errInvalidNVMePolicy)
	}
	result := &NVMeNodePolicy{}
	names := make(map[string]bool)
	for index := range config.Profiles {
		entry := &config.Profiles[index]
		if entry.Name == "" || len(entry.Name) > 64 || names[entry.Name] || entry.Transport != nvmeTransportTCP || entry.Port == 0 || len(entry.Parameters) == 0 {
			return nil, fmt.Errorf("%w: invalid or duplicate profile at index %d", errInvalidNVMePolicy, index)
		}
		names[entry.Name] = true
		profile, err := parseNVMeProfile(entry)
		if err != nil {
			return nil, fmt.Errorf("profile %d: %w", index, err)
		}
		result.profiles = append(result.profiles, profile)
	}
	if err := result.validateOverlap(); err != nil {
		return nil, err
	}
	return result, nil
}

func parseNVMeProfile(entry *nvmePolicyProfileConfig) (nvmeNodeProfile, error) {
	server, err := canonicalNVMeServer(entry.Server)
	if err != nil {
		return nvmeNodeProfile{}, err
	}
	policy, err := parseNVMePolicy(entry.Parameters, true)
	if err != nil {
		return nvmeNodeProfile{}, err
	}
	profile := nvmeNodeProfile{policy: policy, server: server, port: entry.Port, nqns: make(map[string]bool)}
	seen := make(map[netip.Addr]bool)
	for _, address := range entry.Addresses {
		ip, parseErr := netip.ParseAddr(address)
		if parseErr != nil || seen[ip.Unmap()] {
			return nvmeNodeProfile{}, fmt.Errorf("%w: invalid or duplicate pinned address", errInvalidNVMePolicy)
		}
		ip = ip.Unmap()
		seen[ip] = true
		profile.addresses = append(profile.addresses, ip)
	}
	if ip, parseErr := netip.ParseAddr(server); parseErr == nil {
		if len(profile.addresses) == 0 {
			profile.addresses = append(profile.addresses, ip)
		} else if !seen[ip] {
			return nvmeNodeProfile{}, fmt.Errorf("%w: pinned addresses omit the selected IP", errInvalidNVMePolicy)
		}
	} else if len(profile.addresses) == 0 {
		return nvmeNodeProfile{}, fmt.Errorf("%w: hostname profiles require pinned numeric addresses", errInvalidNVMePolicy)
	}
	if entry.NQNs != nil && len(*entry.NQNs) == 0 {
		return nvmeNodeProfile{}, fmt.Errorf("%w: an explicit NQN selector list must not be empty", errInvalidNVMePolicy)
	}
	var nqns []string
	if entry.NQNs != nil {
		nqns = *entry.NQNs
	}
	for _, nqn := range nqns {
		if nqn == "" || len(nqn) > 223 || nqn != strings.TrimSpace(nqn) || profile.nqns[nqn] {
			return nvmeNodeProfile{}, fmt.Errorf("%w: invalid or duplicate NQN selector", errInvalidNVMePolicy)
		}
		profile.nqns[nqn] = true
	}
	return profile, nil
}

func canonicalNVMeServer(server string) (string, error) {
	if strings.HasPrefix(server, "[") || strings.HasSuffix(server, "]") {
		if !strings.HasPrefix(server, "[") || !strings.HasSuffix(server, "]") {
			return "", errInvalidNVMePolicy
		}
		server = strings.TrimSuffix(strings.TrimPrefix(server, "["), "]")
		if ip, err := netip.ParseAddr(server); err != nil || !ip.Is6() {
			return "", errInvalidNVMePolicy
		}
	}
	if ip, err := netip.ParseAddr(server); err == nil {
		return ip.Unmap().String(), nil
	}
	server = strings.TrimSuffix(strings.ToLower(server), ".")
	if server == "" || len(server) > 253 {
		return "", errInvalidNVMePolicy
	}
	for _, label := range strings.Split(server, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errInvalidNVMePolicy
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", errInvalidNVMePolicy
			}
		}
	}
	return server, nil
}

func (p *NVMeNodePolicy) resolve(params *nvmeOFConnectionParams) (nvmePolicy, []netip.Addr, error) {
	resolved := nvmePolicy{}
	var addresses []netip.Addr
	if p == nil || params.transport != nvmeTransportTCP {
		return resolved, addresses, nil
	}
	server, err := canonicalNVMeServer(params.server)
	if err != nil {
		return resolved, nil, err
	}
	port, err := strconv.ParseUint(params.port, 10, 16)
	if err != nil {
		return resolved, nil, errInvalidNVMePolicy
	}
	for index := range p.profiles {
		profile := &p.profiles[index]
		if server != profile.server || port != uint64(profile.port) || (len(profile.nqns) > 0 && !profile.nqns[params.nqn]) {
			continue
		}
		currentParameters := resolved.parameters()
		for key, value := range profile.policy.parameters() {
			if current, present := currentParameters[key]; present && current != value {
				return nvmePolicy{}, nil, fmt.Errorf("%w: conflicting profile matches", errInvalidNVMePolicy)
			}
		}
		resolved = mergeNVMePolicies(profile.policy, resolved)
		if len(addresses) > 0 && !sameNVMeAddresses(addresses, profile.addresses) {
			return nvmePolicy{}, nil, fmt.Errorf("%w: conflicting endpoint pins", errInvalidNVMePolicy)
		}
		addresses = append([]netip.Addr(nil), profile.addresses...)
	}
	return resolved, addresses, nil
}

func (p *NVMeNodePolicy) validateOverlap() error {
	for index := range p.profiles {
		first := &p.profiles[index]
		for other := index + 1; other < len(p.profiles); other++ {
			second := &p.profiles[other]
			if first.server != second.server || first.port != second.port || !nvmeNQNScopesOverlap(first.nqns, second.nqns) {
				continue
			}
			if !sameNVMeAddresses(first.addresses, second.addresses) {
				return fmt.Errorf("%w: overlapping profiles have different endpoint pins", errInvalidNVMePolicy)
			}
			parameters := first.policy.parameters()
			for key, value := range second.policy.parameters() {
				if previous, present := parameters[key]; present && previous != value {
					return fmt.Errorf("%w: overlapping profiles have conflicting parameters", errInvalidNVMePolicy)
				}
			}
		}
	}
	return nil
}

func nvmeNQNScopesOverlap(first, second map[string]bool) bool {
	if len(first) == 0 || len(second) == 0 {
		return true
	}
	for nqn := range first {
		if second[nqn] {
			return true
		}
	}
	return false
}

func sameNVMeAddresses(first, second []netip.Addr) bool {
	if len(first) != len(second) {
		return false
	}
	for index, address := range first {
		if address != second[index] {
			return false
		}
	}
	return true
}

func decodeNVMePolicyJSON(data []byte, value any) error {
	tokens := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueNVMeJSONValue(tokens, 0); err != nil {
		return err
	}
	if _, err := tokens.Token(); !errors.Is(err, io.EOF) {
		return errInvalidNVMePolicy
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: invalid JSON schema", errInvalidNVMePolicy)
	}
	return nil
}

func uniqueNVMeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errInvalidNVMePolicy
	}
	token, err := decoder.Token()
	if err != nil {
		return errInvalidNVMePolicy
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			keyToken, keyErr := decoder.Token()
			key, stringKey := keyToken.(string)
			if keyErr != nil || !stringKey || keys[strings.ToLower(key)] {
				return fmt.Errorf("%w: duplicate or malformed JSON key", errInvalidNVMePolicy)
			}
			keys[strings.ToLower(key)] = true
		}
		if err := uniqueNVMeJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return errInvalidNVMePolicy
	}
	return nil
}
