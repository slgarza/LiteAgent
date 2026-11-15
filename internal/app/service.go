package app

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Main starts the local server or a terminal client with the supplied arguments.
func Main(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	if command == "version" {
		fmt.Println("LiteAgent " + Version)
		return nil
	}
	if command != "serve" && command != "run" && command != "chat" {
		return fmt.Errorf("usage: liteagent [serve|chat|run|version] [options]")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	port := flags.Int("port", 8787, "Local port")
	data := flags.String("data", "", "Data directory (optional, for portable use)")
	noOpen := flags.Bool("no-open", false, "Do not open the web chat automatically")
	chatID := flags.String("chat", "", "Conversation ID to resume (chat mode)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("invalid port")
	}
	address := fmt.Sprintf("127.0.0.1:%d", *port)
	if command == "run" {
		task := strings.Join(flags.Args(), " ")
		if task == "" {
			return fmt.Errorf("usage: liteagent run [--port 8787] \"Your task\"")
		}
		return runCLI("http://"+address, task)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	base := "http://" + address
	if command == "chat" {
		if _, err := fetchCLIState(ctx, base, "new"); err == nil {
			fmt.Println("Connected to LiteAgent at " + base + ". Its server stays running when you exit this chat.")
			return chatCLI(ctx, base, *chatID, os.Stdin, os.Stdout)
		}
	}
	service, err := startLocalService(ctx, address, *data)
	if err != nil {
		return err
	}
	defer service.Close()
	if command == "chat" {
		fmt.Println("Local server started at " + base + ". Keep this terminal open for schedules to run.")
		return chatCLI(ctx, base, *chatID, os.Stdin, os.Stdout)
	}
	fmt.Printf("\nLiteAgent %s\nChat: %s\nData: %s\nPress Ctrl+C to quit.\n\n", Version, base, service.app.dir)
	if !*noOpen {
		openURL(base)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-service.finished:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

type localService struct {
	app      *App
	server   *http.Server
	dataLock *os.File
	cancel   context.CancelFunc
	finished chan error
}

func startLocalService(parent context.Context, address, dir string) (*localService, error) {
	if dir == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(root, "LiteAgent")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Bind before opening storage: a second launch cannot modify an active instance.
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("could not start on %s (is the port already in use?): %w", address, err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		listener.Close()
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		listener.Close()
		return nil, err
	}
	if err := lockDataFile(lock); err != nil {
		lock.Close()
		listener.Close()
		return nil, fmt.Errorf("data directory is already in use or could not be locked: %w", err)
	}
	store, err := openStore(dir)
	if err != nil {
		unlockDataFile(lock)
		lock.Close()
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	app := &App{store: store, browser: &Browser{dir: dir}, dir: dir, root: ctx}
	server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	s := &localService{app: app, server: server, dataLock: lock, cancel: cancel, finished: make(chan error, 1)}
	app.wg.Add(1)
	go func() { defer app.wg.Done(); app.schedulerLoop(ctx) }()
	go func() { s.finished <- server.Serve(listener) }()
	return s, nil
}

func (s *localService) Close() {
	s.cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	if err := s.server.Shutdown(shutdown); err != nil {
		_ = s.server.Close()
	}
	s.app.wg.Wait()
	s.app.browser.Close()
	unlockDataFile(s.dataLock)
	_ = s.dataLock.Close()
}

func openURL(address string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	case "darwin":
		cmd = exec.Command("open", address)
	default:
		cmd = exec.Command("xdg-open", address)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
