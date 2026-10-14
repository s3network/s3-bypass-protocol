package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func TestGetArgsV4Compatible(t *testing.T) {
	originalArgs := os.Args
	defer func() {
		os.Args = originalArgs
	}()

	cases := []struct {
		input []string
		want  []string
	}{
		{
			// no arguments at all: run the default command
			input: []string{"xray"},
			want:  []string{"xray", "run"},
		},
		{
			// an empty argument used to panic with an index out of range
			input: []string{"xray", ""},
			want:  []string{"xray", ""},
		},
		{
			input: []string{"xray", "run", "-c", "config.json"},
			want:  []string{"xray", "run", "-c", "config.json"},
		},
		{
			input: []string{"xray", "-version"},
			want:  []string{"xray", "version"},
		},
		{
			input: []string{"xray", "-c", "config.json"},
			want:  []string{"xray", "run", "-c", "config.json"},
		},
	}

	for _, test := range cases {
		os.Args = test.input
		if got := getArgsV4Compatible(); !reflect.DeepEqual(got, test.want) {
			t.Errorf("getArgsV4Compatible(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestReadConfDirSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	files := []string{"a.json", "b.toml", "c.jsonc"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A subdirectory must not be picked up as a config file, even when its
	// name matches the config extension.
	subDir := filepath.Join(dir, "nested.json")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "d.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file that is not a config at all is ignored as well.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	saved := configFiles
	configFiles = nil
	defer func() {
		configFiles = saved
	}()

	readConfDir(dir)

	want := make([]string, 0, len(files))
	for _, name := range files {
		want = append(want, filepath.Join(dir, name))
	}
	// configFiles is filled in directory order.
	if !reflect.DeepEqual([]string(configFiles), want) {
		t.Errorf("config files = %q, want %q", []string(configFiles), want)
	}
}

func TestGetConfigFilePathFromDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	savedDir, savedFiles := configDir, configFiles
	configDir, configFiles = dir, nil
	defer func() {
		configDir, configFiles = savedDir, savedFiles
	}()

	if got := getConfigFilePath(false); len(got) != 1 || got[0] != filepath.Join(dir, "config.json") {
		t.Errorf("config files = %q, want [%q]", []string(got), filepath.Join(dir, "config.json"))
	}
}

// blockingServer is a server whose Close never returns, like one stuck on a
// connection that does not drain.
type blockingServer struct {
	closed chan struct{}
}

func (s *blockingServer) Start() error { return nil }

func (s *blockingServer) Close() error {
	close(s.closed)
	select {}
}

type instantServer struct {
	closed chan struct{}
}

func (s *instantServer) Start() error { return nil }

func (s *instantServer) Close() error {
	close(s.closed)
	return nil
}

func selfSignal(t *testing.T, sig os.Signal) {
	t.Helper()
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// runShutdown drives waitForShutdown with a pre-registered signal channel, so
// a signal cannot slip in before the handler is in place.
func runShutdown(t *testing.T, server core.Server) <-chan struct{} {
	t.Helper()

	osSignals := make(chan os.Signal, 2)
	signal.Notify(osSignals, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(osSignals) })

	done := make(chan struct{})
	go func() {
		waitForShutdown(server, osSignals)
		close(done)
	}()
	return done
}

func TestWaitForShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sending SIGTERM to the test process is not supported on windows")
	}

	savedTimeout := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	defer func() {
		shutdownTimeout = savedTimeout
	}()

	t.Run("closes the server", func(t *testing.T) {
		server := &instantServer{closed: make(chan struct{})}
		done := runShutdown(t, server)

		selfSignal(t, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("waitForShutdown did not return")
		}
		select {
		case <-server.closed:
		default:
			t.Error("server was not closed")
		}
	})

	t.Run("does not hang on a stuck server", func(t *testing.T) {
		server := &blockingServer{closed: make(chan struct{})}
		done := runShutdown(t, server)

		selfSignal(t, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("waitForShutdown did not return")
		}
	})

	t.Run("second signal forces exit", func(t *testing.T) {
		savedTimeout := shutdownTimeout
		// Far longer than the test is willing to wait.
		shutdownTimeout = time.Hour
		defer func() {
			shutdownTimeout = savedTimeout
		}()

		server := &blockingServer{closed: make(chan struct{})}
		done := runShutdown(t, server)

		selfSignal(t, syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		selfSignal(t, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a second signal did not force the shutdown")
		}
	})
}
