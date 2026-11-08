//go:build !windows

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

// The tests of Up and Down start a stand-in for the gateway: this test binary,
// re-executed with -test.run=^TestHelperFakeGateway$. Nothing is compiled and no
// real gateway runs. The fake answers /health and /api/sandbox/status like the real
// one, records the environment it was given, and obeys the flags below.
//
// A real locked gateway cannot start from `up` until the simchat adapter exists
// (SBX-02): a locked gateway builds no channel but simchat, and the channel manager
// refuses to start with none. TestUpDown_LocalBinary covers that once it can run.

const fakeGatewayMarker = "--fake-gateway"

// fakeConfig makes a Supervisor run the fake with the given extra flags: the test binary
// re-executed as "<pre> gateway --sandbox-locked --fake-gateway <extra>".
func fakeConfig(base string, extra ...string) Config {
	return Config{
		Binary: os.Args[0], TmpDir: base, MinFreeBytes: 1, ReadyTimeout: 30 * time.Second, TermGrace: 2 * time.Second,
		PreArgs:   []string{"-test.run=^TestHelperFakeGateway$", "-test.timeout=2m", "--"},
		ExtraArgs: append([]string{fakeGatewayMarker}, extra...),
	}
}

func newFake(t *testing.T, base string, extra ...string) *Supervisor {
	t.Helper()
	s, err := New(fakeConfig(base, extra...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func hasFlag(name string) bool {
	for _, a := range os.Args {
		if a == name {
			return true
		}
	}
	return false
}

func flagValue(prefix string) string {
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

// TestHelperFakeGateway is the child process. It is a no-op in a normal test run.
func TestHelperFakeGateway(t *testing.T) {
	if os.Getenv("KQ_SANDBOX_LOCKED") != "1" || !hasFlag(fakeGatewayMarker) {
		t.Skip("helper process for the up/down tests")
	}
	home := os.Getenv("KHUNQUANT_HOME")

	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = os.WriteFile(filepath.Join(home, "env-keys.txt"), []byte(strings.Join(keys, "\n")), 0o600)
	_ = os.WriteFile(filepath.Join(home, "argv.txt"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	_, merr := os.Stat(filepath.Join(home, ".kq-sandbox"))
	facts := fmt.Sprintf("marker=%v\nlocked=%s\ninstance=%s\n", merr == nil, os.Getenv("KQ_SANDBOX_LOCKED"), os.Getenv("KQ_SANDBOX_INSTANCE"))
	_ = os.WriteFile(filepath.Join(home, "facts.txt"), []byte(facts), 0o600)
	if out := flagValue("--pidout="); out != "" {
		_ = os.WriteFile(out, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}

	if hasFlag("--echo-token") { // a gateway that logs its token must not leak it through Logs
		fmt.Println("debug: token=" + os.Getenv("KQ_SANDBOX_TOKEN"))
	}
	if hasFlag("--exit-early") {
		fmt.Println("boom: fake gateway refuses to start")
		os.Exit(1)
	}
	if hasFlag("--ignore-term") {
		signal.Ignore(syscall.SIGTERM)
	}

	var cfg struct {
		Gateway struct {
			Port int `json:"port"`
		} `json:"gateway"`
	}
	raw, err := os.ReadFile(os.Getenv("KHUNQUANT_CONFIG"))
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil || cfg.Gateway.Port == 0 {
		fmt.Println("fake gateway: bad config:", err)
		os.Exit(2)
	}
	if _, err := pid.WritePidFile(home, "127.0.0.1", cfg.Gateway.Port); err != nil {
		fmt.Println("fake gateway: pid file:", err)
		os.Exit(2)
	}

	if hasFlag("--hang") {
		time.Sleep(time.Minute) // never serves: the readiness wait must time out
		os.Exit(0)
	}

	locked := !hasFlag("--not-locked")
	// The real gateway answers ready:false until it is fully started: --ready-after-ms=N
	// sends ready:false for the first N ms, --never-ready always, --no-ready-field omits
	// the field as an older gateway would.
	readyAt := time.Now()
	if ms, err := strconv.Atoi(flagValue("--ready-after-ms=")); err == nil {
		readyAt = readyAt.Add(time.Duration(ms) * time.Millisecond)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/sandbox/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+os.Getenv("KQ_SANDBOX_TOKEN") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		st := map[string]any{"enabled": true, "locked": locked, "running": true}
		if !hasFlag("--no-ready-field") {
			st["ready"] = !hasFlag("--never-ready") && !time.Now().Before(readyAt)
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	if hasFlag("--crash-lab") { // the crash group, and a SIGUSR2 that stands for "the armed step was reached"
		armed := ""
		mux.HandleFunc("/api/sandbox/lab/crash", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+os.Getenv("KQ_SANDBOX_TOKEN") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			armed = "c1"
			_ = json.NewEncoder(w).Encode(map[string]string{"id": armed})
		})
		usr2 := make(chan os.Signal, 1)
		signal.Notify(usr2, syscall.SIGUSR2)
		go func() {
			<-usr2
			if armed != "" && !hasFlag("--no-marker") {
				_ = os.MkdirAll(filepath.Join(home, "lab"), 0o700)
				m := `{"arm_id":"c1","point":"after_fn","kind":"dn.open","workflow_id":"wf_1","step":"place_second_leg","attempt":1,"wall_time":"` + time.Now().Format(time.RFC3339Nano) + `"}`
				_ = os.WriteFile(filepath.Join(home, "lab", "crash.json"), []byte(m), 0o600)
			}
			if flagValue("--usr2=") == "exit1" {
				os.Exit(1)
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			time.Sleep(time.Minute)
		}()
	}
	go func() {
		// Like the real gateway (channels.Manager.StartAll): a server that cannot listen
		// ends the process with exit 1.
		err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", cfg.Gateway.Port), mux)
		fmt.Println("Shared HTTP server error:", err)
		os.Exit(1)
	}()

	if hasFlag("--exit3-on-usr1") { // the lock invariant exit of the real gateway
		usr1 := make(chan os.Signal, 1)
		signal.Notify(usr1, syscall.SIGUSR1)
		go func() {
			<-usr1
			fmt.Println("sandbox lock: invariant violated (fake)")
			os.Exit(3)
		}()
	}
	if hasFlag("--ignore-term") {
		select {} // Notify would undo signal.Ignore; only SIGKILL ends this
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	<-term
	pid.RemovePidFile(home)
	os.Exit(0)
}
