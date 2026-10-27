package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

const (
	defaultTermGrace = 15 * time.Second
	defaultMinFreeMB = 1024
	defaultLogTail   = 200
	maxLogTail       = 2000
	maxLogRead       = 1 << 20 // the end of gateway.log that Logs reads
)

var (
	// ErrLowMemory means the memory floor (KQ_SANDBOX_MIN_FREE_MB) stopped an `up`.
	ErrLowMemory = errors.New("supervisor: not enough free memory to start an instance")
	// ErrTooMany means the instance limit is reached.
	ErrTooMany = errors.New("supervisor: instance limit reached")
	// ErrNotLocked means a started gateway did not report an enabled, locked sandbox.
	ErrNotLocked = errors.New("supervisor: instance did not report locked:true")
)

// Config configures New. Every field has a default.
type Config struct {
	Binary       string        // absolute path of the prebuilt khunquant; this executable when empty
	TmpDir       string        // parent of kq-sbx-<name>; os.TempDir() when empty
	MinFreeBytes uint64        // memory floor; KQ_SANDBOX_MIN_FREE_MB (default 1024 MiB) when zero
	MaxInstances int           // 1 when zero (SBX-18 raises it)
	ReadyTimeout time.Duration // 60s when zero, wall time
	TermGrace    time.Duration // SIGTERM grace before SIGKILL; 15s when zero

	// PreArgs and ExtraArgs let a test re-execute its own binary as a fake gateway:
	// they go before "gateway" and after "--sandbox-locked". The supervisor adds
	// "gateway --sandbox-locked" itself, so no setting can start an unlocked gateway.
	PreArgs, ExtraArgs []string
}

// RestartMode says how Restart stops the process. The set is open (SBX-20 adds kill_at).
type RestartMode string

const (
	RestartGraceful RestartMode = "graceful" // SIGTERM, then SIGKILL after TermGrace
	RestartKill     RestartMode = "kill"     // SIGKILL at once
)

// State is where an instance is in its life. The set is open.
type State string

const (
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateRestarting State = "restarting"
	StateExited     State = "exited"
)

// ExitReason says why the previous process ended.
type ExitReason string

const (
	ExitStopped       ExitReason = "stopped"        // Down, or a graceful Restart
	ExitKilled        ExitReason = "killed"         // a kill the supervisor asked for (137)
	ExitLockInvariant ExitReason = "lock_invariant" // exit 3: the gateway refused to run unlocked
	ExitCrashed       ExitReason = "crashed"
)

// Info is the public view of an instance. It carries no token.
type Info struct {
	Name       string     `json:"name"`
	Home       string     `json:"home"`
	GatewayURL string     `json:"gateway_url"`
	LogPath    string     `json:"log"`
	PID        int        `json:"pid"`
	State      State      `json:"state"`
	LastExit   *int       `json:"last_exit,omitempty"`   // exit code of the previous process (137 after a kill)
	ExitReason ExitReason `json:"exit_reason,omitempty"` // why it ended; exit 3 is lock_invariant, not a crash
	StartedAt  time.Time  `json:"started_at"`

	token string
}

// RevealToken is the instance token, for `khunquant sandbox up`, which prints it for
// the developer. Nothing else may call it: the MCP server never does.
func (i Info) RevealToken() string { return i.token }

// instance is one gateway process the supervisor owns.
type instance struct {
	opMu sync.Mutex // serializes up, down and restart of this instance

	// Fixed once the instance is registered.
	name, home, url, token, logPath string
	env                             map[string]string
	client                          *labclient.Client

	mu        sync.Mutex // guards the rest
	p         *proc
	state     State
	asked     ExitReason // why the supervisor is stopping p, so the watcher can say so
	lastExit  *int
	reason    ExitReason
	startedAt time.Time
}

// Supervisor starts, restarts and stops locked sandbox instances of a prebuilt binary.
// It is the only place an instance's token lives.
type Supervisor struct {
	cfg Config

	mu        sync.Mutex
	instances map[string]*instance
}

// New checks the binary and returns a Supervisor. It never compiles anything.
func New(cfg Config) (*Supervisor, error) {
	if runtime.GOOS == "windows" {
		return nil, ErrUnixOnly
	}
	if cfg.Binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("supervisor: locate the khunquant binary: %w", err)
		}
		cfg.Binary = exe
	}
	if st, err := os.Stat(cfg.Binary); err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("supervisor: %q is missing or not executable; run make build", cfg.Binary)
	}
	if cfg.TmpDir == "" {
		cfg.TmpDir = os.TempDir()
	}
	if cfg.MinFreeBytes == 0 {
		mb := uint64(defaultMinFreeMB)
		if v, err := strconv.ParseUint(os.Getenv("KQ_SANDBOX_MIN_FREE_MB"), 10, 64); err == nil {
			mb = v // 0 turns the floor off
		}
		cfg.MinFreeBytes = mb << 20
	}
	cfg.MaxInstances = max(cfg.MaxInstances, 1)
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.TermGrace <= 0 {
		cfg.TermGrace = defaultTermGrace
	}
	return &Supervisor{cfg: cfg, instances: map[string]*instance{}}, nil
}

// Up makes an instance home, starts a locked gateway in it and returns once the gateway
// reports an enabled, locked sandbox. On any failure it stops the gateway and removes
// the home it made.
func (s *Supervisor) Up(ctx context.Context, o UpOptions) (Info, error) {
	name := o.Name
	if name == "" {
		name = "sbx-" + randomHex(4)
	}
	if err := validEnv(o.Env); err != nil {
		return Info{}, err
	}
	home, err := instanceHome(s.cfg.TmpDir, name)
	if err != nil {
		return Info{}, err
	}
	in := &instance{name: name, home: home, env: o.Env, logPath: filepath.Join(home, logName), state: StateStarting}
	in.opMu.Lock() // before it is visible: a Restart or Down must wait for the start
	defer in.opMu.Unlock()
	if err := s.register(in); err != nil {
		return Info{}, err
	}
	ok := false
	defer func() {
		if !ok {
			s.mu.Lock()
			delete(s.instances, name)
			s.mu.Unlock()
		}
	}()

	if err := s.checkMemory(); err != nil {
		return Info{}, err
	}
	// What can fail before a directory exists comes first: a refused start leaves nothing.
	fixtures, err := listFixtures(o.FixturesFrom)
	if err != nil {
		return Info{}, err
	}
	port, err := freePort()
	if err != nil {
		return Info{}, err
	}
	in.token = randomHex(32)
	in.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	if in.client, err = labclient.New(in.url, in.token); err != nil {
		return Info{}, err
	}

	if err := os.Mkdir(home, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Info{}, fmt.Errorf("sandbox up: instance %q already exists at %s; run `khunquant sandbox down %s`", name, home, name)
		}
		return Info{}, fmt.Errorf("sandbox up: create %s: %w", home, err)
	}
	defer func() {
		if !ok {
			_ = os.RemoveAll(home)
		}
	}()
	if err := prepareHome(home, name, port, fixtures); err != nil {
		return Info{}, err
	}
	if err := in.start(ctx, s.cfg); err != nil {
		return Info{}, err
	}
	ok = true
	return in.info(), nil
}

// register adds in to the table unless the name is taken or the limit is reached.
func (s *Supervisor) register(in *instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.instances[in.name]; dup {
		return fmt.Errorf("sandbox up: instance %q is already running in this supervisor", in.name)
	}
	if len(s.instances) >= s.cfg.MaxInstances {
		return fmt.Errorf("%w (%d)", ErrTooMany, s.cfg.MaxInstances)
	}
	s.instances[in.name] = in
	return nil
}

// checkMemory refuses an `up` below the floor. Where free memory is unknown it allows it.
func (s *Supervisor) checkMemory() error {
	if s.cfg.MinFreeBytes == 0 {
		return nil
	}
	free, err := FreeMemory()
	if err != nil || free >= s.cfg.MinFreeBytes {
		return nil
	}
	return fmt.Errorf("%w: %d MiB free, %d MiB required (KQ_SANDBOX_MIN_FREE_MB)", ErrLowMemory, free>>20, s.cfg.MinFreeBytes>>20)
}

// start spawns the gateway of in and waits for it to be ready. On failure the process
// is gone and the error carries the end of the log.
func (in *instance) start(ctx context.Context, cfg Config) error {
	args := append(slices.Clone(cfg.PreArgs), "gateway", "--sandbox-locked")
	args = append(args, cfg.ExtraArgs...)
	p, err := spawn(cfg.Binary, args, childEnv(in.home, in.name, in.token, in.env), in.home)
	if err != nil {
		return err
	}
	in.mu.Lock()
	in.p, in.state, in.startedAt, in.asked = p, StateStarting, time.Now(), ""
	in.mu.Unlock()
	go in.watch(p)

	if err := waitReady(ctx, in.client, p.exited, cfg.ReadyTimeout); err != nil {
		killGroup(p.cmd.Process.Pid)
		_ = p.cmd.Process.Kill()
		<-p.exited
		return fmt.Errorf("sandbox up: %w%s", err, logTail(in.logPath, in.token))
	}
	in.mu.Lock()
	in.state = StateRunning
	select {
	case <-p.exited: // it ended while we set the state; the watcher may have run already
		in.state = StateExited
	default:
	}
	in.mu.Unlock()
	return nil
}

// watch records how p ended. The previous exit stays on the instance after a restart.
func (in *instance) watch(p *proc) {
	<-p.exited
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.p != p {
		return
	}
	code := p.code
	in.lastExit = &code
	switch {
	case in.asked != "":
		in.reason = in.asked
	case code == 3:
		in.reason = ExitLockInvariant
	case code == 0:
		in.reason = ExitStopped
	default:
		in.reason = ExitCrashed
	}
	in.asked = ""
	if in.state == StateRunning {
		in.state = StateExited
	}
}

// stop ends the process of in and waits for it. Graceful sends SIGTERM to the group,
// then SIGKILL after grace; kill sends SIGKILL at once.
func (in *instance) stop(mode RestartMode, grace time.Duration) error {
	in.mu.Lock()
	p := in.p
	in.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case <-p.exited: // already gone: never signal a pid that may have been reused
		return nil
	default:
	}
	in.mu.Lock()
	in.asked = ExitStopped
	if mode == RestartKill {
		in.asked = ExitKilled
	}
	in.mu.Unlock()
	pid := p.cmd.Process.Pid
	if mode != RestartKill {
		termGroup(pid)
		select {
		case <-p.exited:
			return nil
		case <-time.After(grace):
		}
		in.mu.Lock()
		in.asked = ExitKilled
		in.mu.Unlock()
	}
	killGroup(pid)
	select {
	case <-p.exited:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("sandbox: pid %d is still running after SIGKILL", pid)
	}
}

func (in *instance) info() Info {
	in.mu.Lock()
	defer in.mu.Unlock()
	i := Info{
		Name: in.name, Home: in.home, GatewayURL: in.url, LogPath: in.logPath, State: in.state,
		ExitReason: in.reason, StartedAt: in.startedAt, token: in.token,
	}
	if in.p != nil {
		i.PID = in.p.cmd.Process.Pid
	}
	if in.lastExit != nil {
		c := *in.lastExit
		i.LastExit = &c
	}
	return i
}

// lookup finds an instance; an empty name is the only one.
func (s *Supervisor) lookup(name string) (*instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		switch len(s.instances) {
		case 0:
			return nil, errors.New("sandbox: no instance is running; start one first (sandbox_instance op=up)")
		case 1:
		default:
			return nil, fmt.Errorf("sandbox: name the instance (%d are running)", len(s.instances))
		}
		for _, in := range s.instances {
			return in, nil
		}
	}
	in, ok := s.instances[name]
	if !ok {
		return nil, fmt.Errorf("sandbox: no instance %q", name)
	}
	return in, nil
}

// Get returns the Info of one instance (an empty name is the only one).
func (s *Supervisor) Get(name string) (Info, error) {
	in, err := s.lookup(name)
	if err != nil {
		return Info{}, err
	}
	return in.info(), nil
}

// WaitRunning blocks while the instance is starting or restarting and returns once it is
// running. It fails when the instance has exited or ctx ends.
func (s *Supervisor) WaitRunning(ctx context.Context, name string) (Info, error) {
	for {
		i, err := s.Get(name)
		if err != nil {
			return Info{}, err
		}
		switch i.State {
		case StateRunning:
			return i, nil
		case StateExited:
			return i, fmt.Errorf("sandbox: instance %q has exited (%s)", i.Name, i.ExitReason)
		}
		select {
		case <-ctx.Done():
			return Info{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// List returns the instances sorted by name.
func (s *Supervisor) List() []Info {
	s.mu.Lock()
	all := make([]*instance, 0, len(s.instances))
	for _, in := range s.instances {
		all = append(all, in)
	}
	s.mu.Unlock()
	out := make([]Info, 0, len(all))
	for _, in := range all {
		out = append(out, in.info())
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Restart stops the instance as mode says, records the exit, and starts it again in the
// same HOME, on the same port and with the same token. An instance that has already
// exited is started again.
func (s *Supervisor) Restart(ctx context.Context, name string, mode RestartMode) (Info, error) {
	if mode != RestartGraceful && mode != RestartKill {
		return Info{}, fmt.Errorf("sandbox: unknown restart mode %q (graceful or kill)", mode)
	}
	in, err := s.lookup(name)
	if err != nil {
		return Info{}, err
	}
	in.opMu.Lock()
	defer in.opMu.Unlock()
	in.mu.Lock()
	in.state = StateRestarting
	in.mu.Unlock()
	if err := in.stop(mode, s.cfg.TermGrace); err != nil {
		return Info{}, err
	}
	if err := in.start(ctx, s.cfg); err != nil {
		in.mu.Lock()
		in.state = StateExited
		in.mu.Unlock()
		return Info{}, err
	}
	return in.info(), nil
}

// Client is the lab client of the instance (an empty name is the only one). The token
// stays inside it; callers can use the client but not read the token.
func (s *Supervisor) Client(name string) (*labclient.Client, error) {
	in, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	return in.client, nil
}

// Down stops the instance and, unless keepHome, removes its home.
func (s *Supervisor) Down(_ context.Context, name string, keepHome bool) error {
	in, err := s.lookup(name)
	if err != nil {
		return err
	}
	in.opMu.Lock()
	defer in.opMu.Unlock()
	if err := in.stop(RestartGraceful, s.cfg.TermGrace); err != nil {
		return err
	}
	// The process is gone, so this only checks the marker and removes the home.
	if err := DownByName(DownOptions{Name: in.name, KeepHome: keepHome, BaseDir: s.cfg.TmpDir}); err != nil {
		return err
	}
	in.mu.Lock()
	in.state = StateExited
	in.mu.Unlock()
	s.mu.Lock()
	delete(s.instances, in.name)
	s.mu.Unlock()
	return nil
}

// Close downs every instance this supervisor started: they cannot be re-adopted,
// because the token was never stored.
func (s *Supervisor) Close(ctx context.Context) error {
	var errs []error
	for _, i := range s.List() {
		if err := s.Down(ctx, i.Name, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Logs returns the last tail lines of gateway.log (200 by default, at most 2000) that
// contain the given text, with the token masked.
func (s *Supervisor) Logs(name string, tail int, contains string) ([]string, error) {
	in, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if tail <= 0 {
		tail = defaultLogTail
	}
	tail = min(tail, maxLogTail)
	f, err := os.Open(in.logPath)
	if err != nil {
		return nil, fmt.Errorf("sandbox logs: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > maxLogRead {
		_, _ = f.Seek(-maxLogRead, io.SeekEnd)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("sandbox logs: %w", err)
	}
	data = bytes.ReplaceAll(data, []byte(in.token), []byte("***"))
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" && strings.Contains(l, contains) {
			lines = append(lines, l)
		}
	}
	return lines[max(0, len(lines)-tail):], nil
}
