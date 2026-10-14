// kano-updater is installed on the Docker host, never in the application image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoagent"
	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if len(arguments) == 0 {
		return errors.New("usage: kano-updater version|manifest|install|serve|start|recover")
	}
	if arguments[0] == "version" {
		_, err := fmt.Fprintln(output, kanoupdate.AgentVersion)
		return err
	}
	flags := flag.NewFlagSet("kano-updater", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "/etc/kano-updater/config.json", "fixed deployment configuration")
	nginxSite := flags.String("nginx-site", "/etc/nginx/sites-enabled/sub2api", "existing nginx site")
	version := flags.String("version", "", "release version")
	digest := flags.String("digest", "", "expected image digest")
	revision := flags.String("revision", "", "source revision")
	policyPath := flags.String("policy", "deploy/kano-upgrade-policy.json", "reviewed validation policy")
	migrationsPath := flags.String("migrations", "backend/migrations", "source migrations")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid updater arguments")
	}
	if arguments[0] == "manifest" {
		file, err := os.Open(*policyPath)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		var policy kanoupdate.ValidationPolicy
		decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&policy); err != nil {
			return errors.New("invalid migration validation policy")
		}
		manifest, err := kanoupdate.GenerateManifest(os.DirFS(*migrationsPath), policy, *version, *digest, *revision)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(manifest)
	}
	if arguments[0] != "serve" && arguments[0] != "start" && arguments[0] != "recover" && arguments[0] != "install" {
		return errors.New("unknown updater command")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("host updater commands require Linux root")
	}
	config, err := kanoagent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if arguments[0] == "start" {
		client := kanoupdate.NewClient(config.ControlSocket)
		session, err := client.Start(ctx, kanoupdate.StartRequest{Version: *version, ExpectedDigest: *digest, Actor: "host-bootstrap", IdempotencyKey: "bootstrap:" + *version})
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]any{"operation_id": session.Job.ID, "progress_url": session.ProgressURL})
	}
	lock, err := kanoagent.AcquireHostLock(filepath.Join(config.StateDir, "host.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	store, err := kanoagent.OpenStore(config.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if arguments[0] == "install" {
		job, jobErr := store.Current(ctx)
		if jobErr != nil && !errors.Is(jobErr, kanoagent.ErrNotFound) {
			return jobErr
		}
		if job != nil && job.Status.Active() {
			return kanoagent.ErrBusy
		}
		return kanoagent.ConnectDeployment(ctx, config, *nginxSite, kanoagent.CommandRunner{DiagnosticDir: filepath.Join(config.StateDir, "diagnostics")})
	}
	source := kanoupdate.NewGitHubSource()
	engine := &kanoagent.Engine{Config: config, Source: source, Runner: kanoagent.CommandRunner{DiagnosticDir: filepath.Join(config.StateDir, "diagnostics")}}
	if arguments[0] == "recover" {
		job, err := store.Current(ctx)
		if err != nil {
			return err
		}
		if !job.Status.Active() {
			return errors.New("no incomplete upgrade to recover")
		}
		return engine.Execute(ctx, store, job)
	}
	server := kanoagent.NewServer(store, source, engine)
	control, err := kanoagent.ListenUnix(config.ControlSocket, config.ControlGID)
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()
	progress, err := kanoagent.ListenUnix(config.ProgressSocket, config.ProgressGID)
	if err != nil {
		return err
	}
	defer func() { _ = progress.Close() }()
	controlHTTP := &http.Server{Handler: server.ControlHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	progressHTTP := &http.Server{Handler: server.ProgressHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- controlHTTP.Serve(control) }()
	go func() { serverErrors <- progressHTTP.Serve(progress) }()
	workerDone := make(chan struct{})
	go func() { server.Run(ctx); close(workerDone) }()
	select {
	case err = <-serverErrors:
	case <-ctx.Done():
	}
	cancelRun()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = controlHTTP.Shutdown(stopCtx)
	_ = progressHTTP.Shutdown(stopCtx)
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
