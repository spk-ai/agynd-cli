package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func withTmuxPaths(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	t.Cleanup(func(bin, cfg, sock, ti string) func() {
		return func() {
			tmuxBinaryPath, tmuxConfigPath, tmuxSocketDir, tmuxTerminfoPath = bin, cfg, sock, ti
		}
	}(tmuxBinaryPath, tmuxConfigPath, tmuxSocketDir, tmuxTerminfoPath))

	tmuxBinaryPath = filepath.Join(root, "tmux")
	tmuxConfigPath = filepath.Join(root, "tmux.conf")
	tmuxSocketDir = filepath.Join(root, "run")
	tmuxTerminfoPath = filepath.Join(root, "terminfo")
	return root
}

// A missing binary must be survivable: an image whose multiplexer did not
// arrive still serves ephemeral sessions, and losing those to a panic would
// cost the terminal rather than just persistence.
func TestStartShellServerMissingBinaryIsNotFatal(t *testing.T) {
	withTmuxPaths(t)

	called := false
	prev := tmuxCommandContext
	t.Cleanup(func() { tmuxCommandContext = prev })
	tmuxCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		called = true
		return exec.CommandContext(ctx, "true")
	}

	stop := startShellServer(context.Background())
	stop()
	stop()

	if called {
		t.Fatal("started a server with no binary present")
	}
}

func TestStartShellServerInvocation(t *testing.T) {
	withTmuxPaths(t)
	if err := os.WriteFile(tmuxBinaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var argv []string
	var startup *exec.Cmd
	prev := tmuxCommandContext
	t.Cleanup(func() { tmuxCommandContext = prev })
	tmuxCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "true")
		if slices.Contains(args, "start-server") {
			argv, startup = append([]string{name}, args...), cmd
		}
		return cmd
	}

	stop := startShellServer(context.Background())
	t.Cleanup(stop)
	stop()

	want := []string{tmuxBinaryPath, "-L", "agyn", "-f", tmuxConfigPath, "start-server"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if startup == nil || !slices.Contains(startup.Env, "TMUX_TMPDIR="+tmuxSocketDir) {
		t.Fatal("startup did not inherit the private socket environment")
	}

	// The socket directory is normally the init container's to create; agynd
	// creating it as a fallback is what keeps a hand-run container working.
	if info, err := os.Stat(tmuxSocketDir); err != nil || !info.IsDir() {
		t.Fatalf("socket dir not created: %v", err)
	}
}

func TestStartShellServerStartupFailureCanStop(t *testing.T) {
	for _, failure := range []string{"socket-directory", "command"} {
		t.Run(failure, func(t *testing.T) {
			withTmuxPaths(t)
			if err := os.WriteFile(tmuxBinaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if failure == "socket-directory" {
				if err := os.WriteFile(tmuxSocketDir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			prev := tmuxCommandContext
			t.Cleanup(func() { tmuxCommandContext = prev })
			tmuxCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				called = true
				return exec.CommandContext(ctx, "false")
			}
			stop := startShellServer(context.Background())
			stop()
			stop()
			if called != (failure == "command") {
				t.Fatal("startup did not stop at the failed prerequisite")
			}
		})
	}
}

func TestStartShellServerJoinsTitleWorker(t *testing.T) {
	for _, mode := range []string{"explicit-stop", "parent-cancellation"} {
		t.Run(mode, func(t *testing.T) {
			withTmuxPaths(t)
			if err := os.WriteFile(tmuxBinaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			prev := tmuxCommandContext
			t.Cleanup(func() { tmuxCommandContext = prev })
			tmuxCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if slices.Contains(args, "list-clients") {
					select {
					case entered <- ctx:
					default:
					}
					// Hold the actual worker independently of cancellation so the
					// cleanup must join it, not merely cancel its context.
					<-release
				}
				return exec.CommandContext(ctx, "true")
			}
			stop := startShellServer(ctx)
			t.Cleanup(func() { unblock(); stop() })
			var commandCtx context.Context
			select {
			case commandCtx = <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("title worker did not poll the attached clients")
			}
			if mode == "parent-cancellation" {
				cancel()
			}
			stopped := make(chan struct{})
			go func() { stop(); close(stopped) }()
			select {
			case <-commandCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("refresh command did not receive cancellation")
			}
			select {
			case <-stopped:
				t.Fatal("cleanup returned before the title worker exited")
			case <-time.After(25 * time.Millisecond):
			}
			unblock()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("cleanup did not join the released title worker")
			}
			stop()
			if mode == "explicit-stop" && ctx.Err() != nil {
				t.Fatal("title cleanup canceled its caller")
			}
		})
	}
}

// The socket must not land on tmux's default path, or an engineer running
// their own tmux inside a shell joins the platform's server and silently
// loses their own configuration to it.
func TestShellServerEnvNamesPrivateSocketDir(t *testing.T) {
	withTmuxPaths(t)

	if !slices.Contains(shellServerEnv(), "TMUX_TMPDIR="+tmuxSocketDir) {
		t.Fatalf("TMUX_TMPDIR not set to %s", tmuxSocketDir)
	}
}

// TERMINFO_DIRS supplements rather than replaces: the trailing empty element
// is what ncurses reads as "and the compiled-in defaults too", so an image
// carrying a good database of its own keeps it.
func TestShellServerEnvTerminfoOnlyWhenPresent(t *testing.T) {
	withTmuxPaths(t)

	hasTerminfo := func() bool {
		for _, kv := range shellServerEnv() {
			if strings.HasPrefix(kv, "TERMINFO_DIRS=") {
				if !strings.HasSuffix(kv, ":") {
					t.Fatalf("TERMINFO_DIRS must end in an empty element, got %q", kv)
				}
				return true
			}
		}
		return false
	}

	if hasTerminfo() {
		t.Fatal("TERMINFO_DIRS set with no tree delivered")
	}

	if err := os.MkdirAll(tmuxTerminfoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasTerminfo() {
		t.Fatal("TERMINFO_DIRS not set with a tree present")
	}
}
