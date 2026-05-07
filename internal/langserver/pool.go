// Package langserver spawns and tracks language_server_linux_x64 child
// processes — one per unique outbound egress (proxy). Matches
// src/langserver.js. Each entry also caches the per-LS session id and the
// one-shot Cascade workspace init future.
package langserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"windsurfapi/internal/convpool"
	"windsurfapi/internal/logx"
	"windsurfapi/internal/windsurf"
)

// DefaultAPIServer is the upstream Codeium endpoint baked into LS args.
const DefaultAPIServer = "https://server.self-serve.windsurf.com"

// DefaultCSRF is a 64-char [A-Za-z0-9] token generated ONCE per process and
// shared between the Go proxy and the spawned LS via --csrf_token. Historical
// deployments used a hardcoded literal, which meant a source-code leak was
// equivalent to a credential leak. With per-boot randomness, an attacker who
// wants to speak to 127.0.0.1:42100 must first read our running process table
// to learn the token — a local-only exposure rather than a public-source one.
var DefaultCSRF = randomCSRF(64)

func randomCSRF(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	max := big.NewInt(int64(len(alphabet)))
	b := make([]byte, n)
	for i := range b {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand.Int fails only when the kernel entropy source is
			// dead — unrecoverable; better to crash than run with a weak token.
			panic("langserver: csrf random failed: " + err.Error())
		}
		b[i] = alphabet[x.Int64()]
	}
	return string(b)
}

// Proxy describes an HTTP CONNECT egress.
type Proxy struct {
	Type     string // "http" / "socks" (socks not yet honoured)
	Host     string
	Port     int
	Username string
	Password string
}

// Entry is one running LS process.
type Entry struct {
	Process   *os.Process
	Port      int
	CSRF      string
	Proxy     *Proxy
	StartedAt time.Time
	Ready     bool

	// stdinKeeper holds the write end of an open pipe connected to the LS's
	// stdin. The LS manager process watches stdin for EOF — if we let it
	// default to /dev/null the LS self-exits within a second of spawn
	// (interprets EOF as "parent IDE gone"). Keeping this file open mirrors
	// what the JS spawn(..., {stdio:['pipe',...]}) gives us for free.
	stdinKeeper *os.File

	// SessionID is per-LS and stable across requests — cached so a single LS
	// can keep a warm workspace state.
	SessionID string

	// Warmup is guarded by warmMu; warmDone flips to true once the three-RPC
	// init succeeds and flips back to false when a PANEL_STATE_NOT_FOUND
	// forces us to redo it.
	warmMu   sync.Mutex
	warmDone bool
}

// Warmup runs fn exactly once until ResetWarmup() is called. fn is expected
// to be idempotent on the wire — if it fails no state is kept.
func (e *Entry) Warmup(fn func() error) error {
	e.warmMu.Lock()
	defer e.warmMu.Unlock()
	if e.warmDone {
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	e.warmDone = true
	return nil
}

// ResetWarmup clears the warmup gate (and the session id) so the next call
// re-runs the three-RPC sequence against a fresh sessionID.
func (e *Entry) ResetWarmup() {
	e.warmMu.Lock()
	e.warmDone = false
	e.SessionID = windsurf.NewSessionID()
	e.warmMu.Unlock()
}

// Pool is the global LS registry.
type Pool struct {
	mu         sync.Mutex
	binary     string
	apiServer  string
	entries    map[string]*Entry // proxyKey → Entry
	nextPort   int
	// spawning serialises concurrent Ensure() on the same proxyKey. Without
	// it, two simultaneous requests for px_foo_8080 would both miss the
	// entry lookup, each consume a different nextPort, each spawn an LS,
	// and only the last write to `entries[key]` would win — the other LS
	// becomes an unmanaged orphan with no StopAll hook. Closing the chan
	// releases waiters.
	spawning map[string]chan struct{}
	// stoppingAll flips to true inside StopAll() so the cmd.Wait exit
	// watchers know to NOT respawn. Without it, tearing the pool down at
	// shutdown would race into "LS died unexpectedly → respawn" and leave
	// a dangling LS process after the Go service exits. Lives under mu.
	stoppingAll bool
}

// New creates an empty pool. Call Config to set binary path + API server,
// then StartWatchdog() once at boot to enable the periodic health probe.
func New() *Pool {
	return &Pool{
		entries:  map[string]*Entry{},
		spawning: map[string]chan struct{}{},
		nextPort: 42101,
	}
}

// StartWatchdog spins up a goroutine that periodically probes every pool
// entry's port. Complements the cmd.Wait exit watcher:
//
//   - cmd.Wait only fires when the LS process actually terminates. A
//     hung-but-alive LS (deadlock, accept queue exhausted, kernel-level
//     stuck in D state) keeps the process around but stops responding.
//   - Adopted entries (no Process reference) have no cmd.Wait at all.
//   - A silent LS exit where cmd.Wait hasn't fired yet (kernel still
//     reaping the zombie) would leave a window where the port is dead but
//     our pool still marks it Ready.
//
// The watchdog closes all three. Every probeInterval, for each Ready
// entry we do a lightweight `isPortInUse` loopback check; a port that
// stays dead across two consecutive probes triggers cleanup (delete
// entry + convpool invalidate) and schedules an Ensure to respawn. The
// cmd.Wait watcher's auto-respawn handles the live-process-then-crash
// path; this handles the "already dead by the time we looked" and
// "process alive but port unresponsive" cases.
func (p *Pool) StartWatchdog() {
	go p.watchdogLoop()
}

const (
	probeInterval = 30 * time.Second
	probeDeadline = 2 // consecutive misses before we declare dead
)

func (p *Pool) watchdogLoop() {
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	missCount := map[string]int{}
	for range t.C {
		p.mu.Lock()
		if p.stoppingAll {
			p.mu.Unlock()
			return
		}
		// Snapshot (key, entry) pairs under the lock; probe without it.
		type ref struct {
			key   string
			entry *Entry
		}
		snaps := make([]ref, 0, len(p.entries))
		for k, e := range p.entries {
			if e.Ready {
				snaps = append(snaps, ref{k, e})
			}
		}
		p.mu.Unlock()

		// Track which keys we saw this round — missing keys (entry was
		// removed between snapshots) should have their miss counters
		// forgotten so a later respawn starts fresh.
		seen := map[string]bool{}
		for _, r := range snaps {
			seen[r.key] = true
			if isPortInUse(r.entry.Port) {
				missCount[r.key] = 0
				continue
			}
			missCount[r.key]++
			if missCount[r.key] < probeDeadline {
				logx.Debug("LS watchdog: %s port %d miss #%d", r.key, r.entry.Port, missCount[r.key])
				continue
			}
			// Dead. Kill the process (if any) so cmd.Wait fires and the
			// auto-respawn path takes over. If there's no Process ref
			// (adopted entry, or hung stale), we need to clean the entry
			// ourselves and Ensure a replacement.
			logx.Warn("LS watchdog: %s port %d dead after %d probes — replacing", r.key, r.entry.Port, missCount[r.key])
			delete(missCount, r.key)
			if r.entry.Process != nil {
				_ = r.entry.Process.Kill()
				// cmd.Wait goroutine will now fire, remove entry, and
				// respawn. No further action needed here.
				continue
			}
			// Adopted entry — no cmd.Wait. Remove + respawn directly.
			p.mu.Lock()
			if p.entries[r.key] == r.entry {
				delete(p.entries, r.key)
			}
			p.mu.Unlock()
			convpool.InvalidateFor("", r.entry.Port)
			go func(px *Proxy, key string) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := p.Ensure(ctx, px); err != nil {
					logx.Error("LS watchdog respawn %s failed: %s", key, err.Error())
				}
			}(r.entry.Proxy, r.key)
		}
		// Garbage-collect miss counters for keys that no longer exist.
		for k := range missCount {
			if !seen[k] {
				delete(missCount, k)
			}
		}
	}
}

// Config sets binary + api server. Safe to call before Ensure().
func (p *Pool) Config(binary, apiServer string) {
	if binary != "" {
		p.binary = binary
	}
	if apiServer != "" {
		p.apiServer = apiServer
	}
}

func proxyKey(px *Proxy) string {
	if px == nil || px.Host == "" {
		return "default"
	}
	return fmt.Sprintf("px_%s_%d", strings.ReplaceAll(px.Host, ".", "_"), px.Port)
}

func proxyURL(px *Proxy) string {
	if px == nil || px.Host == "" {
		return ""
	}
	auth := ""
	if px.Username != "" {
		auth = fmt.Sprintf("%s:%s@", url.QueryEscape(px.Username), url.QueryEscape(px.Password))
	}
	port := px.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://%s%s:%d", auth, px.Host, port)
}

// killOrphanOnPort's implementation is platform-specific (kill_linux.go /
// kill_other.go) because syscall.Kill exists only on Unix. The dispatch
// here stays small so non-Linux builds compile cleanly.

// proxyLabel is the logger-safe form of proxyURL — just host:port, no
// credentials. Use this anywhere the output lands in stdout / the ring
// buffer / log files so a full proxy URL with username:password doesn't end
// up in a log rotation or SSE stream.
func proxyLabel(px *Proxy) string {
	if px == nil || px.Host == "" {
		return "none"
	}
	port := px.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("%s:%d", px.Host, port)
}

func isPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitPortReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("LS port %d not ready after %v", port, timeout)
}

// Ensure spawns (or returns an existing) Entry for the given proxy.
func (p *Pool) Ensure(ctx context.Context, px *Proxy) (*Entry, error) {
	key := proxyKey(px)
	for {
		p.mu.Lock()
		if e, ok := p.entries[key]; ok && e.Ready {
			p.mu.Unlock()
			return e, nil
		}
		// Another goroutine is already spawning this key — park on the
		// shared channel and retry once it signals.
		if ch, inFlight := p.spawning[key]; inFlight {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ch:
				// Winner finished; loop to read the entry (or retry on failure).
			}
			continue
		}
		// We're the winner — claim the slot and drop the lock to do the
		// slow work (spawn + port-ready poll). Close the channel on exit
		// so parked goroutines proceed.
		done := make(chan struct{})
		p.spawning[key] = done
		defer func() {
			p.mu.Lock()
			delete(p.spawning, key)
			p.mu.Unlock()
			close(done)
		}()
		break
	}

	isDefault := key == "default"
	port := p.nextPort
	if isDefault {
		port = 42100
	} else {
		p.nextPort++
	}

	// Orphan LS on our default port (42100)? The previous "adopt" strategy
	// attached our fresh DefaultCSRF to somebody else's process — but the
	// orphan spawned with a DIFFERENT random CSRF, so every gRPC call
	// returned `permission_denied` and the whole pool failed closed.
	//
	// Instead: best-effort evict the squatter and fall through to spawn a
	// fresh LS under our own CSRF. fuser/pkill aren't in $PATH on minimal
	// images, so we rely on SO_REUSEADDR from the new process and a short
	// delay for the kernel to reap the TIME_WAIT — `Ensure`'s spawn path
	// below will still get the port even if the orphan lingers briefly.
	if isDefault && isPortInUse(port) {
		logx.Warn("LS default port %d already in use — orphan likely, trying to free it before spawning", port)
		p.mu.Unlock()
		killOrphanOnPort(port)
		// Give the kernel up to 2 s to release the socket.
		deadline := time.Now().Add(2 * time.Second)
		for isPortInUse(port) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if isPortInUse(port) {
			return nil, fmt.Errorf("LS port %d still held by an orphan process and could not be freed", port)
		}
		p.mu.Lock()
	}
	p.mu.Unlock()

	// Use local workspace directory instead of /opt/windsurf/data
	dataDir := fmt.Sprintf("./windsurf-workspace/%s", key)
	_ = os.MkdirAll(dataDir+"/db", 0o755)

	args := []string{
		"--api_server_url=" + p.apiServer,
		fmt.Sprintf("--server_port=%d", port),
		"--csrf_token=" + DefaultCSRF,
		"--register_user_url=https://api.codeium.com/register_user/",
		"--codeium_dir=" + dataDir,
		"--database_dir=" + dataDir + "/db",
		"--enable_local_search=false",
		"--enable_index_service=false",
		"--enable_lsp=false",
		"--detect_proxy=false",
	}

	// IMPORTANT: use plain exec.Command, NOT CommandContext. The caller's ctx
	// is a short-lived "spawn + ready" timeout; if we bind the process to it,
	// the LS gets SIGKILL the moment ctx.cancel() runs (typically right after
	// Ensure returns). The LS should outlive the spawn ctx and live for the
	// lifetime of the pool — StopAll() signals termination explicitly.
	cmd := exec.Command(p.binary, args...)
	// Use actual user home instead of hardcoded /root
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/tmp"
	}
	env := append(os.Environ(), "HOME="+homeDir)
	if pu := proxyURL(px); pu != "" {
		env = append(env, "HTTPS_PROXY="+pu, "HTTP_PROXY="+pu, "https_proxy="+pu, "http_proxy="+pu)
	}
	cmd.Env = env

	// Give the LS an always-open stdin pipe (see Entry.stdinKeeper comment).
	// Without this the LS manager sees EOF on /dev/null and self-exits.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.Stdin = stdinR

	// Pipe stdout/stderr through to our structured logger so a misbehaving LS
	// is debuggable. Without this the LS's logs vanish into /dev/null.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("spawn LS: %w", err)
	}
	// The parent no longer needs the read-end of stdin or the write-ends of
	// stdout/stderr — the child has them now.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	entry := &Entry{
		Process:     cmd.Process,
		Port:        port,
		CSRF:        DefaultCSRF,
		Proxy:       px,
		StartedAt:   time.Now(),
		SessionID:   windsurf.NewSessionID(),
		stdinKeeper: stdinW,
	}

	go pipeLSOutput(key, "stdout", stdoutR)
	go pipeLSOutput(key, "stderr", stderrR)

	// Watch for exit so we can drop the pool entry + invalidate cascade
	// reuse. If the exit was unexpected (LS crashed rather than being
	// asked to stop), schedule a respawn so the proxy doesn't silently
	// sit idle until the next chat request. Previously we relied on lazy
	// `Ensure` from the hot path to detect and respawn; but after a
	// startup-time LS crash (e.g. TIME_WAIT race on :42100 after a tight
	// service restart, or LS's internal manager→worker self-connect
	// timing out once), nothing would trigger a retry and /health would
	// keep returning 200 while every chat request failed.
	go func() {
		waitErr := cmd.Wait()
		_ = stdinW.Close()
		p.mu.Lock()
		stopping := p.stoppingAll
		// Only drop the entry if it's still the one WE created — a
		// respawn racing with us would have replaced it already.
		if cur, ok := p.entries[key]; ok && cur == entry {
			delete(p.entries, key)
		}
		p.mu.Unlock()
		convpool.InvalidateFor("", entry.Port)

		if stopping {
			logx.Info("LS instance %s exited cleanly (shutdown)", key)
			return
		}
		if waitErr == nil {
			// Clean exit without StopAll — rare but benign (e.g. external
			// `kill <pid>`). Treat like a crash and respawn.
			logx.Info("LS instance %s exited cleanly unexpectedly — respawning", key)
		} else {
			logx.Warn("LS instance %s exited: %s — respawning", key, waitErr.Error())
		}
		// Small backoff so a tight-loop crash (misconfigured binary, missing
		// CAP_NET_BIND, etc.) doesn't fork-bomb the host. Port TIME_WAIT
		// on the same :42100 is the usual culprit immediately after a
		// previous exit, and a 2-second wait gives the kernel room to
		// reap before the next bind.
		go func(px *Proxy) {
			time.Sleep(2 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := p.Ensure(ctx, px); err != nil {
				logx.Error("LS instance %s respawn failed: %s", key, err.Error())
			} else {
				logx.Info("LS instance %s respawned", key)
			}
		}(entry.Proxy)
	}()

	// Never log the CSRF token (or any prefix) — it's a per-process secret.
	// Also never log proxyURL() — it embeds username:password in the URL form.
	// host:port is enough for operators to tell instances apart.
	logx.Info("Starting LS instance key=%s port=%d proxy=%s", key, port, proxyLabel(px))
	p.mu.Lock()
	p.entries[key] = entry
	p.mu.Unlock()

	if err := waitPortReady(port, 25*time.Second); err != nil {
		_ = cmd.Process.Kill()
		// Close the stdin pipe explicitly. In the happy path this is
		// handled by the cmd.Wait() goroutine at line 363, but if
		// cmd.Process.Kill itself fails (process already reaped, PID
		// recycled, permission error) we can leak the pipe's file
		// descriptor indefinitely. Safe to close twice — os.File.Close
		// just returns already-closed error which we ignore.
		if entry.stdinKeeper != nil {
			_ = entry.stdinKeeper.Close()
		}
		p.mu.Lock()
		delete(p.entries, key)
		p.mu.Unlock()
		return nil, err
	}
	entry.Ready = true
	logx.Info("LS instance %s ready on port %d", key, port)
	return entry, nil
}

// Get returns the Entry matching px (or the default LS as fallback).
func (p *Pool) Get(px *Proxy) *Entry {
	key := proxyKey(px)
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		return e
	}
	return p.entries["default"]
}

// GetByPort walks every entry for the one with matching port.
func (p *Pool) GetByPort(port int) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.Port == port {
			return e
		}
	}
	return nil
}

// Snapshot returns a read-only view of the pool — matches getLsStatus().
type Snapshot struct {
	Running      bool              `json:"running"`
	PID          int               `json:"pid"`
	Port         int               `json:"port"`
	StartedAt    int64             `json:"startedAt"`
	RestartCount int               `json:"restartCount"`
	Instances    []InstanceView    `json:"instances"`
}

type InstanceView struct {
	Key       string `json:"key"`
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Proxy     string `json:"proxy,omitempty"`
	StartedAt int64  `json:"startedAt"`
	Ready     bool   `json:"ready"`
}

func (p *Pool) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	def := p.entries["default"]
	s := Snapshot{Running: len(p.entries) > 0}
	if def != nil {
		if def.Process != nil {
			s.PID = def.Process.Pid
		}
		s.Port = def.Port
		s.StartedAt = def.StartedAt.UnixMilli()
	}
	for k, e := range p.entries {
		iv := InstanceView{Key: k, Port: e.Port, StartedAt: e.StartedAt.UnixMilli(), Ready: e.Ready}
		if e.Process != nil {
			iv.PID = e.Process.Pid
		}
		if e.Proxy != nil {
			iv.Proxy = fmt.Sprintf("%s:%d", e.Proxy.Host, e.Proxy.Port)
		}
		s.Instances = append(s.Instances, iv)
	}
	return s
}

// StopAll interrupts every tracked LS and waits for the underlying port to
// free before returning. Without this wait, a back-to-back
// StopAll → Ensure (as fired by /dashboard/api/langserver/restart) races
// with the dying LS: Ensure's isPortInUse check still sees the port held
// by our own shutting-down process and takes the "adopt" branch, creating
// a phantom Entry with no Process. Subsequent chat requests then hit a
// dead port. Waiting here guarantees the next Ensure spawns fresh.
func (p *Pool) StopAll() {
	p.mu.Lock()
	// stoppingAll guards the cmd.Wait exit watcher from firing an auto-
	// respawn for LSes we're deliberately killing right now. Set under
	// the same lock that triggers the signals so by the time any victim
	// actually exits and its watcher takes the lock, the flag is visibly
	// true. Reset at the end of StopAll so genuine post-StopAll crashes
	// (dashboard restart path → explicit Ensure → crash) still auto-respawn.
	p.stoppingAll = true
	type victim struct {
		key  string
		proc *os.Process
		port int
	}
	victims := make([]victim, 0, len(p.entries))
	for key, e := range p.entries {
		if e.stdinKeeper != nil {
			_ = e.stdinKeeper.Close()
		}
		if e.Process != nil {
			_ = e.Process.Signal(os.Interrupt)
			victims = append(victims, victim{key: key, proc: e.Process, port: e.Port})
		}
		logx.Info("LS instance %s stopped", key)
	}
	p.entries = map[string]*Entry{}
	p.mu.Unlock()

	// Wait (outside the lock) for each port to free. 5s grace; SIGKILL the
	// stragglers and poll once more. Fast path: most LSes exit in <500ms.
	for _, v := range victims {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && isPortInUse(v.port) {
			time.Sleep(100 * time.Millisecond)
		}
		if isPortInUse(v.port) && v.proc != nil {
			logx.Warn("LS %s did not release port %d after SIGINT; sending SIGKILL", v.key, v.port)
			_ = v.proc.Kill()
			end := time.Now().Add(2 * time.Second)
			for time.Now().Before(end) && isPortInUse(v.port) {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	p.mu.Lock()
	p.stoppingAll = false
	p.mu.Unlock()
}

// isBenignLSNoise is true for LS stderr lines that look like errors or
// warnings but are fully handled by our recovery paths, or are part of a
// normal shutdown. Surfacing them at their klog level in the log panel
// creates a false impression of failure when the Go side has already
// re-warmed / ignored them / intentionally signalled the process.
//
//   - "panel state not found"           → Cascade panel GC'd between
//     requests; client.go runs ResetWarmup() + re-open and retries.
//   - "path is already tracked"         → AddTrackedWorkspace re-registers a
//     path LS already knows about; LS rejects as a duplicate but continues
//     to track it correctly, so the request succeeds.
//   - "Got signal terminated" /          → Normal shutdown chatter. systemd
//     "Received terminated" /              stop (or our StopAll) delivers
//     "Got signal interrupt" /             SIGTERM/SIGINT → LS's klog echoes
//     "initiating shutdown"                these. They're expected whenever
//                                          the service stops or the user
//                                          clicks "重启 LS" in the dashboard.
func isBenignLSNoise(text string) bool {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "panel state not found"),
		strings.Contains(lower, "path is already tracked"),
		strings.Contains(lower, "got signal terminated"),
		strings.Contains(lower, "got signal interrupt"),
		strings.Contains(lower, "received terminated"),
		strings.Contains(lower, "initiating shutdown"),
		strings.Contains(lower, "shutting down"):
		return true
	}
	return false
}

// pipeLSOutput forwards the LS child process's stdout/stderr line-by-line
// into our structured logger. LS lines shaped like `Iymmdd hh:mm:ss.us ...`
// come from klog and are generally INFO; everything else also goes to debug
// so the SSE log stream can surface problems.
func pipeLSOutput(key, stream string, r *os.File) {
	defer r.Close()
	buf := make([]byte, 4096)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			carry = append(carry, buf[:n]...)
			for {
				idx := -1
				for i, b := range carry {
					if b == '\n' {
						idx = i
						break
					}
				}
				if idx < 0 {
					break
				}
				line := carry[:idx]
				carry = carry[idx+1:]
				if len(line) == 0 {
					continue
				}
				// Trim trailing \r for Windows-style lines.
				if line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				text := string(line)
				// Classify by first char (klog convention). LS's klog emits
				// a handful of "errors" that are purely informational from
				// our perspective — the Go side has explicit recovery paths
				// for them, so surfacing them as ERROR misleads operators
				// reading the log panel. Downgrade those to DEBUG.
				if isBenignLSNoise(text) {
					logx.Debug("[LS:%s:%s] %s", key, stream, text)
					break
				}
				switch {
				case len(text) > 0 && text[0] == 'E':
					logx.Error("[LS:%s:%s] %s", key, stream, text)
				case len(text) > 0 && text[0] == 'W':
					logx.Warn("[LS:%s:%s] %s", key, stream, text)
				default:
					logx.Debug("[LS:%s:%s] %s", key, stream, text)
				}
			}
		}
		if err != nil {
			if len(carry) > 0 {
				logx.Debug("[LS:%s:%s] %s", key, stream, string(carry))
			}
			return
		}
	}
}
