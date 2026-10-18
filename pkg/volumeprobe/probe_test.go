package volumeprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test executable supplies helper modes without any storage dependencies.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == HelperArgument {
		data, _ := io.ReadAll(io.LimitReader(os.Stdin, maxRequest+1))
		var request Request
		if json.Unmarshal(data, &request) != nil {
			os.Exit(2)
		}
		switch request.Filesystem {
		case "test-hold":
			holdTestHelper(request.Path)
			os.Exit(0)
		case "test-descendant":
			binary, _ := os.Executable()
			cmd := exec.CommandContext(context.Background(), binary, HelperArgument)
			next, marshalErr := json.Marshal(Request{Path: request.Path, Filesystem: "test-hold"})
			if marshalErr != nil {
				os.Exit(2)
			}
			cmd.Stdin = bytes.NewReader(next)
			cmd.ExtraFiles = []*os.File{os.NewFile(3, "global"), os.NewFile(4, "volume")}
			if cmd.Start() != nil {
				os.Exit(2)
			}
			waitTestStarted(request.Path)
			fmt.Print(`{"code":"unknown","version":1}`)
			os.Exit(0)
		case "test-overflow":
			fmt.Print(strings.Repeat("x", maxOutput+1))
			os.Exit(0)
		case "test-malformed":
			fmt.Print(`{"code":"ok","version":2}`)
			os.Exit(0)
		case "test-error":
			os.Exit(7)
		case "test-env":
			if os.Getenv("TRUENAS_API_KEY") != "" || os.Getenv("PATH") != "/usr/sbin:/usr/bin:/sbin:/bin" {
				os.Exit(3)
			}
			fmt.Print(`{"code":"ok","version":1}`)
			os.Exit(0)
		}
		os.Exit(RunHelper(context.Background(), bytes.NewReader(data), os.Stdout))
	}
	os.Exit(m.Run())
}

func holdTestHelper(path string) {
	if os.WriteFile(path+".started", []byte("started"), 0o600) != nil {
		os.Exit(2)
	}
	for range 1000 {
		if _, err := os.Stat(path + ".release"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(2)
}

func waitTestStarted(path string) {
	for range 1000 {
		if _, err := os.Stat(path + ".started"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(2)
}

func testRequest(path, fs string) Request {
	return Request{Path: path, Filesystem: fs, MountID: 1, Device: "0:1"}
}

func TestRequestAndOutputBounds(t *testing.T) {
	for _, data := range []string{`{}`, `{"path":"relative"}`, strings.Repeat("x", maxRequest+1), `{"path":"/a","device":"0:1","mountID":1,"filesystem":"xfs","surprise":1}`, `{"path":"/a","device":"0:1","mountID":1,"filesystem":"xfs"} {}`} {
		if _, err := decodeRequest(strings.NewReader(data)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted %q: %v", data, err)
		}
	}
	valid, marshalErr := json.Marshal(testRequest("/a", "xfs"))
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if _, err := decodeRequest(bytes.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	var output limitedOutput
	n, err := output.Write(bytes.Repeat([]byte("a"), maxOutput+10))
	data, overflow := output.contents()
	if err != nil || n != maxOutput+10 || !overflow || len(data) != maxOutput {
		t.Fatal("output was not bounded")
	}
}

func TestRunnerRejectsFailureAndSanitizesEnvironment(t *testing.T) {
	t.Setenv("TRUENAS_API_KEY", "test-sentinel-must-not-reach-helper")
	runner := newTestRunner(t, t.TempDir())
	for _, fs := range []string{"test-overflow", "test-malformed", "test-error"} {
		if _, err := runner.Probe(context.Background(), "v", testRequest(t.TempDir(), fs)); err == nil {
			t.Fatalf("accepted %s", fs)
		}
	}
	if result, err := runner.Probe(context.Background(), "v", testRequest(t.TempDir(), "test-env")); err != nil || result.Code != OK {
		t.Fatalf("environment/result: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner.executable = func() (string, error) { t.Fatal("launched a canceled request"); return "", nil }
	if _, err := runner.Probe(ctx, "v", testRequest("/a", "test-env")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProbeLeasesSurviveTimeoutAndNewRunner(t *testing.T) {
	directory := t.TempDir()
	runner := newTestRunner(t, directory)
	runner.timeout = 100 * time.Millisecond
	runner.kill = func(*exec.Cmd) error { return nil }
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), "hold")
		request := testRequest(paths[i], "test-hold")
		start := time.Now()
		if _, err := runner.Probe(context.Background(), strconv.Itoa(i), request); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probe %d: %v", i, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("RPC waited for child exit")
		}
		volume := strconv.Itoa(i)
		t.Cleanup(func() { releaseTestProbe(t, directory, volume, request.Path) })
	}
	restarted := newTestRunner(t, directory)
	for _, volume := range []string{"0", "new-volume"} {
		if _, err := restarted.Probe(context.Background(), volume, testRequest("/a", "test-env")); !errors.Is(err, ErrBusy) {
			t.Fatalf("admitted %s while children still hold leases: %v", volume, err)
		}
	}
	for _, path := range paths {
		if err := os.WriteFile(path+".release", nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := restarted.Probe(context.Background(), "0", testRequest("/a", "test-env"))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) < 8 {
		t.Fatalf("lease files were unlinked: %v %v", files, err)
	}
}

func TestNestedChildRetainsInheritedLeases(t *testing.T) {
	directory, path := t.TempDir(), filepath.Join(t.TempDir(), "descendant")
	t.Cleanup(func() { releaseTestProbe(t, directory, "v", path) })
	runner := newTestRunner(t, directory)
	result, err := runner.Probe(context.Background(), "v", testRequest(path, "test-descendant"))
	if err != nil || result.Code != Unknown {
		t.Fatalf("supervisor: %+v %v", result, err)
	}
	if _, err := runner.Probe(context.Background(), "v", testRequest("/a", "test-env")); !errors.Is(err, ErrBusy) {
		t.Fatalf("nested child lost its leases: %v", err)
	}
}

func TestLeaseFailuresAndLaunchFailuresReleaseAdmission(t *testing.T) {
	directory := t.TempDir()
	runner := newTestRunner(t, directory)
	runner.executable = func() (string, error) { return "/nonexistent/probe", nil }
	if _, err := runner.Probe(context.Background(), "v", testRequest("/a", "xfs")); err == nil {
		t.Fatal("missing executable accepted")
	}
	runner.executable = nil
	if _, err := runner.Probe(context.Background(), "v", testRequest("/a", "test-env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "global-0.lock"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Probe(context.Background(), "v", testRequest("/a", "test-env")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("permissive lease accepted: %v", err)
	}
}

func releaseTestProbe(t *testing.T, directory, volume, path string) {
	t.Helper()
	if err := os.WriteFile(path+".release", nil, 0o600); err != nil {
		t.Error(err)
		return
	}
	runner := newTestRunner(t, directory)
	deadline := time.Now().Add(3 * time.Second)
	for {
		global, local, err := runner.acquire(volume)
		if err == nil {
			closeLease(global)
			closeLease(local)
			return
		}
		if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
			t.Errorf("helper did not release its lease: %v", err)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeadlineKillsOrdinaryHelperAndReleasesItsLeases(t *testing.T) {
	directory, path := t.TempDir(), filepath.Join(t.TempDir(), "ordinary-helper")
	runner := newTestRunner(t, directory)
	runner.timeout = 100 * time.Millisecond
	if _, err := runner.Probe(context.Background(), "v", testRequest(path, "test-hold")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	runner.timeout = 3 * time.Second
	for {
		_, err := runner.Probe(context.Background(), "v", testRequest("/a", "test-env"))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestRunner(t *testing.T, directory string) Runner {
	t.Helper()
	runner := Runner{Directory: directory}
	// Instrumented self-exec helpers must write into the test binary's own
	// coverage directory; production helpers retain the sanitized environment.
	if coverDir := flag.Lookup("test.gocoverdir"); coverDir != nil && coverDir.Value.String() != "" {
		path := coverDir.Value.String()
		runner.environment = func() []string { return append(probeEnvironment(), "GOCOVERDIR="+path) }
	}
	return runner
}
