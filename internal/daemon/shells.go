package daemon

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The tmux server behind persistent shells.
//
// agynd starts it rather than letting the first client spawn it, for two
// reasons. A tmux server captures its environment from whoever starts it, and
// shells created hours apart must not differ by which client happened to
// arrive first. And a server already running is one less thing for an attach
// to do at the moment a person is waiting on it.
//
// Nothing above the container knows tmux is what implements a shell. The
// Terminal Proxy binds an attach-or-create command and the clients name a
// shell by an opaque id, which is what keeps the engine replaceable.

const (
	tmuxBinary = agentBinPath + "/tmux"
	tmuxConfig = "/agyn/tmux.conf"

	// Off the default socket. An engineer running plain tmux inside a shell
	// gets their own server and their own ~/.tmux.conf: tmux configuration is
	// server-wide, so sharing one would silently discard theirs.
	tmuxSocketName = "agyn"

	// The socket directory. Written by the init container, because the main
	// container may run as a uid that cannot create it; agynd creates it only
	// as a fallback. The image's own /tmp is not used — it may be read-only,
	// and the platform should not depend on it either way.
	tmuxTmpDir = "/agyn/run"

	// The curated terminfo tree, for the programs running inside a pane. tmux
	// carries the entries it needs compiled in; vim and htop do their own
	// lookup and may find no database at all in a slim image.
	//
	// The trailing empty element is read by ncurses as "and the compiled-in
	// defaults too", so an image with a good database of its own keeps it.
	// TERMINFO_DIRS rather than TERMINFO for the same reason: it supplements
	// the primary location rather than replacing it.
	tmuxTerminfoDirs = "/agyn/terminfo:"

	// start-server daemonizes and returns; this only bounds a binary that
	// hangs rather than starting.
	tmuxStartTimeout = 15 * time.Second

	// How often each attached client is asked to re-read its title.
	//
	// tmux evaluates set-titles-string only when it redraws a client, and its
	// own periodic redraw is the status line's timer -- which the platform
	// turns off. So a shell that changes directory goes on announcing the old
	// one until something else redraws it, and for a browser tab that means
	// until the person clicks into the terminal. This stands in for the timer
	// the status line would have provided.
	titleRefreshInterval = time.Second

	// A tmux client talking to a server on the same socket; a timeout here only
	// bounds one that has stopped answering.
	tmuxCommandTimeout = 5 * time.Second
)

var (
	tmuxCommandContext = exec.CommandContext

	// Swapped in tests. The values above are what runs in a container.
	tmuxBinaryPath   = tmuxBinary
	tmuxConfigPath   = tmuxConfig
	tmuxSocketDir    = tmuxTmpDir
	tmuxTerminfoPath = "/agyn/terminfo"
)

// startShellServer brings up the tmux server holding this container's
// persistent shells.
//
// Failure is logged and not fatal. A workload whose server did not start still
// serves ephemeral sessions, which is what every consumer that does not ask
// for a shell already uses — so a broken multiplexer costs persistence, not
// the terminal. The returned cleanup stops and joins the title worker; it
// deliberately does not terminate the tmux server or its persistent shells.
func startShellServer(ctx context.Context) func() {
	if _, err := os.Stat(tmuxBinaryPath); err != nil {
		log.Printf("shell server not started: %s unavailable: %v", tmuxBinaryPath, err)
		return func() {}
	}
	if err := os.MkdirAll(tmuxSocketDir, 0o777); err != nil {
		log.Printf("shell server not started: socket dir %s: %v", tmuxSocketDir, err)
		return func() {}
	}

	startCtx, cancel := context.WithTimeout(ctx, tmuxStartTimeout)
	defer cancel()

	cmd := tmuxCommandContext(startCtx, tmuxBinaryPath, "-L", tmuxSocketName, "-f", tmuxConfigPath, "start-server")
	cmd.Env = shellServerEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("shell server not started: %v: %s", err, strings.TrimSpace(string(out)))
		return func() {}
	}
	log.Printf("shell server started on socket %q", tmuxSocketName)

	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshShellTitles(refreshCtx)
	}()
	return func() {
		cancelRefresh()
		<-done
	}
}

// refreshShellTitles keeps what each shell announces about itself current.
//
// Without it a tab names the directory the shell was in when something last
// redrew it, which is not the same as the directory the shell is in.
func refreshShellTitles(ctx context.Context) {
	ticker := time.NewTicker(titleRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshAttachedClients(ctx)
		}
	}
}

// refreshAttachedClients asks tmux to re-evaluate the title of every client.
//
// -S redraws the status line rather than the screen: the title is recomputed
// on that path too, so this costs a few bytes a second instead of repainting
// every pane. A container with nothing attached costs one listing and no
// writes.
//
// Every failure is ignored. The server may not be running, and a title that
// did not refresh is a stale tab, which is what this is already fixing.
func refreshAttachedClients(ctx context.Context) {
	listCtx, cancel := context.WithTimeout(ctx, tmuxCommandTimeout)
	defer cancel()
	list := tmuxCommandContext(listCtx, tmuxBinaryPath, "-L", tmuxSocketName, "list-clients", "-F", "#{client_tty}")
	list.Env = shellServerEnv()
	out, err := list.Output()
	if err != nil {
		return
	}

	// One invocation for every client: ";" separates commands within a single
	// tmux run, so the cost does not grow with the number of open tabs.
	args := []string{"-L", tmuxSocketName}
	for _, tty := range strings.Fields(string(out)) {
		if len(args) > 2 {
			args = append(args, ";")
		}
		args = append(args, "refresh-client", "-S", "-t", tty)
	}
	if len(args) == 2 {
		return
	}

	refreshCtx, cancelRefresh := context.WithTimeout(ctx, tmuxCommandTimeout)
	defer cancelRefresh()
	refresh := tmuxCommandContext(refreshCtx, tmuxBinaryPath, args...)
	refresh.Env = shellServerEnv()
	_ = refresh.Run()
}

// shellServerEnv is what every shell in this container inherits, because the
// server is agynd's child and its children are the shells.
//
// This is the difference from an ephemeral session, which comes from
// Runner.Exec and inherits the container spec. Variables on the spec reach
// both — agynd is started from the same spec — so nothing the Orchestrator
// injects is lost either way.
func shellServerEnv() []string {
	env := os.Environ()
	env = append(env, "TMUX_TMPDIR="+tmuxSocketDir)
	if _, err := os.Stat(tmuxTerminfoPath); err == nil {
		env = append(env, "TERMINFO_DIRS="+tmuxTerminfoPath+":")
	}
	return env
}
