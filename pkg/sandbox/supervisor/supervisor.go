package supervisor

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

const (
	defaultTermGrace   = 15 * time.Second
	defaultRestartWait = 60 * time.Second
	defaultMinFreeMB   = 1024
	defaultLogTail     = 200
	// MaxInstancesCeiling is the most instances one supervisor may run, whatever
	// KQ_SANDBOX_MAX_INSTANCES says.
	MaxInstancesCeiling = 8
	// DefaultMaxSnapshots is how many snapshots the store holds before a new take is
	// refused; KQ_SANDBOX_MAX_SNAPSHOTS overrides it, clamped to [1, MaxSnapshotsCeiling].
	DefaultMaxSnapshots = 20
	MaxSnapshotsCeiling = 200
	maxLogTail          = 2000
	maxLogRead          = 1 << 20 // the end of gateway.log that Logs reads
)

var (
	// ErrLowMemory means the memory floor (KQ_SANDBOX_MIN_FREE_MB) stopped an `up`.
	ErrLowMemory = errors.New("supervisor: not enough free memory to start an instance")
	// ErrTooMany means the instance limit is reached.
	ErrTooMany = errors.New("supervisor: instance limit reached")
	// ErrInstanceRequired means several instances run and the call named none: there is no
	// implicit default, so a call never reaches the wrong one.
	ErrInstanceRequired = errors.New("supervisor: several instances are running; pass instance")
	// ErrNameTaken means a running instance already has this name.
	ErrNameTaken = errors.New("supervisor: an instance with this name exists")
	// ErrNotLocked means a started gateway did not report an enabled, locked sandbox.
	ErrNotLocked = errors.New("supervisor: instance did not report locked:true")
)

// Config configures New. Every field has a default.
type Config struct {
	Binary       string        // absolute path of the prebuilt khunquant; this executable when empty
	TmpDir       string        // parent of kq-sbx-<name>; os.TempDir() when empty
	MinFreeBytes uint64        // memory floor; KQ_SANDBOX_MIN_FREE_MB (default 1024 MiB) when zero
	MaxInstances int           // KQ_SANDBOX_MAX_INSTANCES, else 1; clamped to [1, MaxInstancesCeiling]
	MaxSnapshots int           // KQ_SANDBOX_MAX_SNAPSHOTS, else DefaultMaxSnapshots; clamped to [1, MaxSnapshotsCeiling]
	ReadyTimeout time.Duration // 60s when zero, wall time
	// RestartWait is how long a call through this supervisor's client waits for an instance
	// that is restarting (wall time; 60s when zero). A `restart kill_at` can set its own.
	RestartWait time.Duration
	TermGrace   time.Duration // SIGTERM grace before SIGKILL; 15s when zero
	// Getenv is where a record or live start reads its key, allowlist and prices (os.Getenv
	// when nil). A test supplies its own, so it never sees the developer's shell.
	Getenv func(string) string

	// PreArgs and ExtraArgs let a test re-execute its own binary as a fake gateway:
	// they go before "gateway" and after "--sandbox-locked". The supervisor adds
	// "gateway --sandbox-locked" itself, so no setting can start an unlocked gateway.
	PreArgs, ExtraArgs []string
}

// RestartMode says how Restart stops the process. The set is open.
type RestartMode string

const (
	RestartGraceful RestartMode = "graceful" // SIGTERM, then SIGKILL after TermGrace
	RestartKill     RestartMode = "kill"     // SIGKILL at once
	// RestartKillAt arms a crash point (Supervisor.KillAt): the instance dies when a step
	// reaches it, and is restarted by the supervisor. It is not a mode of Restart.
	RestartKillAt RestartMode = "kill_at"
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
	// ExitCrashPoint: SIGKILL (code 137) with a crash marker, the death `restart kill_at`
	// armed. The supervisor restarts the instance.
	ExitCrashPoint ExitReason = "crash_point"
	// ExitUnexpectedKill: SIGKILL with no marker and none asked for (the OOM killer, a stray
	// kill -9). It is reported, and the instance is not restarted as if it were planned.
	ExitUnexpectedKill ExitReason = "unexpected_kill"
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
	// Stale is a home with the instance marker whose process is gone (a supervisor that
	// died). It is listed, never adopted: its token is unknown by design. Down removes it.
	Stale bool `json:"stale,omitempty"`
	// Crash is the last crash at a point (the marker the process left); CrashRestarts counts
	// them; Armed is the crash point armed and not yet fired; RestartError is why an
	// automatic restart after a crash failed.
	Crash         *crash.Marker `json:"crash,omitempty"`
	CrashRestarts int           `json:"crash_restarts,omitempty"`
	Armed         string        `json:"armed,omitempty"`
	RestartError  string        `json:"restart_error,omitempty"`

	token string
}

// RevealToken is the instance token, for `khunquant sandbox up`, which prints it for
// the developer. Nothing else may call it: the MCP server never does.
func (i Info) RevealToken() string { return i.token }

// instance is one gateway process the supervisor owns.
type instance struct {
	opMu sync.Mutex // serializes up, down and restart of this instance

	// Fixed once the instance is registered.
	name, home, token, logPath string
	env                        map[string]string
	cfg                        Config // the supervisor's, for the restart after a crash

	// url, port and client change with each attempt of Up, under mu; a Restart keeps them.
	url    string
	port   int
	client *labclient.Client

	mu        sync.Mutex // guards the rest
	p         *proc
	state     State
	asked     ExitReason // why the supervisor is stopping p, so the watcher can say so
	lastExit  *int
	reason    ExitReason
	startedAt time.Time

	// A restart after a crash at a point (killat.go).
	restarting    bool          // the instance is down and about to be, or being, started again
	armed         string        // the crash point armed and not yet fired
	wait          time.Duration // wait_wall of the last `restart kill_at`; 0 takes cfg.RestartWait
	crash         *crash.Marker
	crashRestarts int
	restartErr    string
	// tokenSpent: a restart's readiness poll reached a socket that was not the gateway (the
	// port was taken), so the token may be known to it. The instance cannot be restarted.
	tokenSpent bool
}

// Supervisor starts, restarts and stops locked sandbox instances of a prebuilt binary.
// It is the only place an instance's token lives.
type Supervisor struct {
	cfg Config

	// pickPort chooses a gateway's port: freePort, except where a test injects one.
	pickPort func() (int, error)
	// freeMem reads free memory: FreeMemory, except where a test injects one.
	freeMem func() (uint64, error)

	mu        sync.Mutex
	instances map[string]*instance
	// snapMu serializes Snapshot across instances, so the count check against the cap and
	// the move into the store are one step.
	snapMu sync.Mutex
	// restoreMu keeps a snapshot in the store from a restore's check of it to the end of the
	// restore: Restore reads it, DeleteSnapshot writes it. It is taken with no other lock
	// held, and Snapshot never takes it, so it comes before opMu and snapMu on every path.
	restoreMu sync.RWMutex
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
	if cfg.MaxInstances == 0 {
		cfg.MaxInstances, _ = strconv.Atoi(os.Getenv("KQ_SANDBOX_MAX_INSTANCES")) // garbage reads as 0
	}
	cfg.MaxInstances = min(max(cfg.MaxInstances, 1), MaxInstancesCeiling)
	if cfg.MaxSnapshots == 0 {
		cfg.MaxSnapshots, _ = strconv.Atoi(os.Getenv("KQ_SANDBOX_MAX_SNAPSHOTS")) // garbage reads as 0
	}
	if cfg.MaxSnapshots == 0 {
		cfg.MaxSnapshots = DefaultMaxSnapshots
	}
	cfg.MaxSnapshots = min(max(cfg.MaxSnapshots, 1), MaxSnapshotsCeiling)
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.TermGrace <= 0 {
		cfg.TermGrace = defaultTermGrace
	}
	if cfg.RestartWait <= 0 {
		cfg.RestartWait = defaultRestartWait
	}
	return &Supervisor{cfg: cfg, pickPort: freePort, freeMem: FreeMemory, instances: map[string]*instance{}}, nil
}

// Up makes an instance home, starts a locked gateway in it and returns once the gateway
// reports an enabled, locked sandbox. On any failure it stops the gateway and removes
// the home it made.
func (s *Supervisor) Up(ctx context.Context, o UpOptions) (Info, error) {
	name := o.Name
	if name == "" {
		name = "sbx-" + randomHex(4)
	}
	env, home, err := s.upInputs(o, name)
	if err != nil {
		return Info{}, err
	}
	in := &instance{name: name, home: home, env: env, logPath: filepath.Join(home, logName), state: StateStarting, cfg: s.cfg}
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
	for attempt := 1; ; attempt++ {
		err := s.attempt(ctx, in, o, fixtures)
		if err == nil {
			break
		}
		// Only a port taken between choosing it and binding it is tried again; any other
		// failure is the single attempt it always was.
		var taken *portTakenError
		if !errors.As(err, &taken) {
			return Info{}, err
		}
		if attempt == maxStartAttempts {
			taken.tries = attempt
			return Info{}, taken
		}
		in.forgetFailedStart()
	}
	ok = true
	return in.info(), nil
}

// upInputs checks the options of an `up` called name and returns the child's environment
// and the home's path. It touches nothing.
func (s *Supervisor) upInputs(o UpOptions, name string) (map[string]string, string, error) {
	if err := validEnv(o.Env); err != nil {
		return nil, "", err
	}
	env, err := o.childEnvOf(name, s.cfg.Getenv)
	if err != nil {
		return nil, "", err
	}
	if err := checkOverlay(o.ConfigPatch, o.Workspace); err != nil {
		return nil, "", err
	}
	if err := cmp.Or(CheckAccounts(o.Accounts), CheckAccountPatch(o.ConfigPatch, o.Accounts)); err != nil {
		return nil, "", err
	}
	home, err := instanceHome(s.cfg.TmpDir, name)
	if err != nil {
		return nil, "", err
	}
	return env, home, nil
}

// attempt is one try of Up: a new port, a client and URL for it, a home made from
// scratch, and a start. On failure the home it made is gone again.
func (s *Supervisor) attempt(ctx context.Context, in *instance, o UpOptions, fixtures []fixtureFile) (err error) {
	port, err := s.reservePort(in)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	// A fresh token per attempt (175-1): a lost port is held by another socket, and the
	// readiness poll sends it this attempt's token as a bearer, so that token must never
	// become the instance's. Only a restart keeps a token (see tokenSpent).
	token := randomHex(32)
	client, err := labclient.New(url, token)
	if err != nil {
		return err
	}
	client.SetGate(in.gate) // a call waits out a restart (killat.go)
	in.mu.Lock()
	in.token = token
	in.url, in.client = url, client
	in.mu.Unlock()

	if err := os.Mkdir(in.home, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("sandbox up: instance %q already exists at %s; run `khunquant sandbox down %s`", in.name, in.home, in.name)
		}
		return fmt.Errorf("sandbox up: create %s: %w", in.home, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(in.home)
		}
	}()
	if err := prepareHome(in.home, in.name, port, fixtures, o.Accounts...); err != nil {
		return err
	}
	if o.restore != nil { // every attempt: a retry makes a new home and may have a new port
		if err := o.restore.apply(in.home, port); err != nil {
			return err
		}
	}
	if len(o.ConfigPatch) > 0 {
		if _, err := patchConfigFile(in.home, o.ConfigPatch); err != nil {
			return fmt.Errorf("sandbox up: config patch: %w", err)
		}
	}
	if err := writeWorkspace(in.home, o.Workspace); err != nil {
		return fmt.Errorf("sandbox up: workspace: %w", err)
	}
	return in.start(ctx, s.cfg)
}

// reservePort picks a loopback port that no other instance of this supervisor holds and
// records it on in. The kernel may hand the same ephemeral port to two starts that overlap,
// since the first has not bound it yet; a Down frees the reservation with the instance.
func (s *Supervisor) reservePort(in *instance) (int, error) {
	for range 8 {
		port, err := s.pickPort()
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		held := false
		for _, o := range s.instances {
			if o != in {
				o.mu.Lock()
				held = held || o.port == port
				o.mu.Unlock()
			}
		}
		if !held {
			in.mu.Lock()
			in.port = port
			in.mu.Unlock()
		}
		s.mu.Unlock()
		if !held {
			return port, nil
		}
	}
	return 0, errors.New("sandbox up: no loopback port free of the other instances")
}

// forgetFailedStart clears what a failed attempt recorded, so the attempt after it does
// not report the dead process's exit as the instance's own. It drops p first: a
// watcher that has not run yet then sees another process and records nothing.
func (in *instance) forgetFailedStart() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.p, in.lastExit, in.reason, in.asked = nil, nil, "", ""
}

// register adds in to the table unless the name is taken or the limit is reached.
func (s *Supervisor) register(in *instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.instances[in.name]; dup {
		return fmt.Errorf("%w: %q is already running in this supervisor", ErrNameTaken, in.name)
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
	free, err := s.freeMem()
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
	if in.restarting {
		in.state = StateRestarting // it is a restart until the new process is ready
	}
	in.mu.Unlock()
	go in.watch(p)

	in.mu.Lock()
	client, port := in.client, in.port
	in.mu.Unlock()
	if err := waitReady(ctx, client.Ungated(), p.exited, cfg.ReadyTimeout); err != nil {
		killGroup(p.cmd.Process.Pid)
		_ = p.cmd.Process.Kill()
		<-p.exited
		if errors.Is(err, errGatewayExited) && logShowsPortTaken(in.logPath, port) {
			return &portTakenError{port: port, tail: logTail(in.logPath, in.token)}
		}
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
	resume := false
	switch {
	case in.asked != "":
		in.reason = in.asked
	case code == 3:
		in.reason = ExitLockInvariant
	case code == 0:
		in.reason = ExitStopped
	case code == exitSIGKILL:
		// SIGKILL nobody here asked for: a crash at a point when the process left its
		// marker, else an unexpected kill (reported, not restarted).
		if m := readMarker(in.home); m != nil && !m.WallTime.Before(in.startedAt) { // not one an earlier process left
			in.reason, in.crash, resume = ExitCrashPoint, m, true
			in.crashRestarts++
		} else {
			in.reason = ExitUnexpectedKill
		}
	default:
		in.reason = ExitCrashed
	}
	in.asked, in.armed = "", ""
	switch {
	case resume:
		in.state, in.restarting = StateRestarting, true
		killGroup(p.cmd.Process.Pid) // selfKill ended the gateway only: its children must not outlive it
		go in.resume(p)
	case in.state == StateRunning:
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
		Crash: in.crash, CrashRestarts: in.crashRestarts, Armed: in.armed, RestartError: in.restartErr,
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

// lookup finds an instance. An empty name is the only one, and only while exactly one
// runs: with several there is no default, and the error names them.
func (s *Supervisor) lookup(name string) (*instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		switch len(s.instances) {
		case 0:
			return nil, errors.New("sandbox: no instance is running; start one first (sandbox_instance op=up)")
		case 1:
			for _, in := range s.instances {
				return in, nil
			}
		}
		return nil, fmt.Errorf("%w (`name` on sandbox_instance); running: %s", ErrInstanceRequired, s.namesLocked())
	}
	in, ok := s.instances[name]
	if !ok {
		return nil, fmt.Errorf("sandbox: no instance %q (running: %s)", name, s.namesLocked())
	}
	return in, nil
}

func (s *Supervisor) namesLocked() string {
	if len(s.instances) == 0 {
		return "none"
	}
	return strings.Join(slices.Sorted(maps.Keys(s.instances)), ", ")
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
			if in, lerr := s.lookup(name); lerr == nil && in.dead() { // died; the watcher has not classified it yet
				break
			}
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

// List returns the instances sorted by name, then the stale homes (Stale: true).
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
	stale, _ := scanStale(s.cfg.TmpDir, s.runningNames())
	return append(out, stale...)
}

func (s *Supervisor) runningNames() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(s.instances))
	for n := range s.instances {
		set[n] = true
	}
	return set
}

// Restart stops the instance as mode says, records the exit, and starts it again in the
// same HOME, on the same port and with the same token. An instance that has already
// exited is started again.
func (s *Supervisor) Restart(ctx context.Context, name string, mode RestartMode) (Info, error) {
	if mode == RestartKillAt {
		return Info{}, errors.New("sandbox: restart kill_at arms a crash point and returns; it is Supervisor.KillAt, not a Restart")
	}
	if mode != RestartGraceful && mode != RestartKill {
		return Info{}, fmt.Errorf("sandbox: unknown restart mode %q (graceful, kill or kill_at)", mode)
	}
	in, err := s.lookup(name)
	if err != nil {
		return Info{}, err
	}
	in.opMu.Lock()
	defer in.opMu.Unlock()
	in.mu.Lock()
	if in.tokenSpent {
		in.mu.Unlock()
		return Info{}, fmt.Errorf("sandbox: instance %q cannot be restarted: its port was taken during an earlier restart, so its token may have reached another socket; run down and up", in.name)
	}
	in.state, in.restarting = StateRestarting, true
	in.mu.Unlock()
	defer func() { // calls wait for the restart, whichever way it ends
		in.mu.Lock()
		in.restarting = false
		in.mu.Unlock()
	}()
	if err := in.stop(mode, s.cfg.TermGrace); err != nil {
		return Info{}, err
	}
	if err := in.start(ctx, s.cfg); err != nil {
		in.mu.Lock()
		in.state = StateExited
		in.tokenSpent = errors.Is(err, errPortTaken)
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
	in.mu.Lock()
	defer in.mu.Unlock()
	// Listed from the start of Up, an instance has no client yet, or one for a gateway
	// nobody has checked answers locked and ready (143-1, 144-2). A restart is not this:
	// its state is StateRestarting and the client's gate holds the call (killat.go).
	if in.client == nil || in.state == StateStarting {
		return nil, fmt.Errorf("sandbox: instance %q is still starting; try again when it is running", in.name)
	}
	return in.client, nil
}

// Down stops the instance and, unless keepHome, removes its home.
func (s *Supervisor) Down(_ context.Context, name string, keepHome bool) error {
	in, err := s.lookup(name)
	if err != nil {
		// A stale home (its process is gone) is removed, not adopted.
		if stale, _ := scanStale(s.cfg.TmpDir, s.runningNames()); name != "" && slices.ContainsFunc(stale, func(i Info) bool { return i.Name == name }) {
			return DownByName(DownOptions{Name: name, KeepHome: keepHome, BaseDir: s.cfg.TmpDir})
		}
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
// because the token was never stored. They go down concurrently, each with its own
// SIGTERM grace then SIGKILL, so eight stubborn ones take one grace, not eight.
func (s *Supervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	names := slices.Sorted(maps.Keys(s.instances))
	s.mu.Unlock()
	var (
		wg   sync.WaitGroup
		emu  sync.Mutex
		errs []error
	)
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Down(ctx, n, false); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		}()
	}
	wg.Wait()
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
	in.mu.Lock()
	token := in.token
	in.mu.Unlock()
	data = bytes.ReplaceAll(data, []byte(token), []byte("***"))
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" && strings.Contains(l, contains) {
			lines = append(lines, l)
		}
	}
	return lines[max(0, len(lines)-tail):], nil
}
