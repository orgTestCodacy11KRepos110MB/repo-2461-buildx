//go:build linux

package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/docker/buildx/build"
	"github.com/docker/buildx/commands/controller"
	controllerapi "github.com/docker/buildx/commands/controller/pb"
	"github.com/docker/buildx/monitor"
	"github.com/docker/buildx/util/confutil"
	"github.com/docker/buildx/version"
	"github.com/docker/cli/cli/command"
	"github.com/moby/buildkit/client"
	"github.com/pelletier/go-toml"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

const (
	serveCommandName = "_INTERNAL_SERVE"
)

type serverConfig struct {
	// Specify buildx server root
	Root string `toml:"root"`

	// LogLevel sets the logging level [trace, debug, info, warn, error, fatal, panic]
	LogLevel string `toml:"log_level"`

	// Specify file to output buildx server log
	LogFile string `toml:"log_file"`
}

func newRemoteBuildxController(ctx context.Context, dockerCli command.Cli, opts buildOptions) (monitor.BuildxController, error) {
	rootDir := opts.root
	if rootDir == "" {
		rootDir = rootDataDir(dockerCli)
	}
	serverRoot := filepath.Join(rootDir, "shared")
	c, err := newBuildxClientAndCheck(filepath.Join(serverRoot, "buildx.sock"), 1, 0)
	if err != nil {
		logrus.Info("no buildx server found; launching...")
		// start buildx server via subcommand
		launchFlags := []string{}
		if opts.serverConfig != "" {
			launchFlags = append(launchFlags, "--config", opts.serverConfig)
		}
		logFile, err := getLogFilePath(dockerCli, opts.serverConfig)
		if err != nil {
			return nil, err
		}
		wait, err := launch(ctx, logFile, append([]string{serveCommandName}, launchFlags...)...)
		if err != nil {
			return nil, err
		}
		go wait()
		c, err = newBuildxClientAndCheck(filepath.Join(serverRoot, "buildx.sock"), 10, time.Second)
		if err != nil {
			return nil, fmt.Errorf("cannot connect to the buildx server: %w", err)
		}
	}
	return &buildxController{c, serverRoot}, nil
}

func addControllerCommands(cmd *cobra.Command, dockerCli command.Cli, rootOpts *rootOptions) {
	cmd.AddCommand(
		serveCmd(dockerCli, rootOpts),
	)
}

func serveCmd(dockerCli command.Cli, rootOpts *rootOptions) *cobra.Command {
	var serverConfigPath string
	cmd := &cobra.Command{
		Use:    fmt.Sprintf("%s [OPTIONS]", serveCommandName),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse config
			config, err := getConfig(dockerCli, serverConfigPath)
			if err != nil {
				return fmt.Errorf("failed to get config")
			}
			if config.LogLevel == "" {
				logrus.SetLevel(logrus.InfoLevel)
			} else {
				lvl, err := logrus.ParseLevel(config.LogLevel)
				if err != nil {
					return fmt.Errorf("failed to prepare logger: %w", err)
				}
				logrus.SetLevel(lvl)
			}
			logrus.SetFormatter(&logrus.JSONFormatter{
				TimestampFormat: log.RFC3339NanoFixed,
			})
			root, err := prepareRootDir(dockerCli, config)
			if err != nil {
				return err
			}
			pidF := filepath.Join(root, "pid")
			if err := os.WriteFile(pidF, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
				return err
			}
			defer func() {
				if err := os.Remove(pidF); err != nil {
					logrus.Errorf("failed to clean up info file %q: %v", pidF, err)
				}
			}()

			// prepare server
			b := controller.New(func(ctx context.Context, options *controllerapi.BuildOptions, stdin io.Reader, statusChan chan *client.SolveStatus) (res *build.ResultContext, err error) {
				return runBuildWithContext(ctx, dockerCli, *options, stdin, "quiet", statusChan)
			})
			defer b.Close()

			// serve server
			addr := filepath.Join(root, "buildx.sock")
			if err := os.Remove(addr); err != nil && !os.IsNotExist(err) { // avoid EADDRINUSE
				return err
			}
			defer func() {
				if err := os.Remove(addr); err != nil {
					logrus.Errorf("failed to clean up socket %q: %v", addr, err)
				}
			}()
			logrus.Infof("starting server at %q", addr)
			l, err := net.Listen("unix", addr)
			if err != nil {
				return err
			}
			rpc := grpc.NewServer()
			controllerapi.RegisterControllerServer(rpc, b)
			doneCh := make(chan struct{})
			errCh := make(chan error, 1)
			go func() {
				defer close(doneCh)
				if err := rpc.Serve(l); err != nil {
					errCh <- fmt.Errorf("error on serving via socket %q: %w", addr, err)
				}
				return
			}()
			var s os.Signal
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			select {
			case s = <-sigCh:
				logrus.Debugf("got signal %v", s)
			case err := <-errCh:
				return err
			case <-doneCh:
			}
			return nil

		},
	}

	flags := cmd.Flags()
	flags.StringVar(&serverConfigPath, "config", "", "Specify buildx server config file")
	return cmd
}

func getLogFilePath(dockerCli command.Cli, configPath string) (string, error) {
	config, err := getConfig(dockerCli, configPath)
	if err != nil {
		return "", fmt.Errorf("failed to get config")
	}
	logFile := config.LogFile
	if logFile == "" {
		root, err := prepareRootDir(dockerCli, config)
		if err != nil {
			return "", err
		}
		logFile = filepath.Join(root, "log")
	}
	return logFile, nil
}

func getConfig(dockerCli command.Cli, configPath string) (*serverConfig, error) {
	var defaultConfigPath bool
	if configPath == "" {
		defaultRoot := rootDataDir(dockerCli)
		configPath = filepath.Join(defaultRoot, "config.toml")
		defaultConfigPath = true
	}
	var config serverConfig
	tree, err := toml.LoadFile(configPath)
	if err != nil && !(os.IsNotExist(err) && defaultConfigPath) {
		return nil, fmt.Errorf("failed to load config file %q", configPath)
	} else if err == nil {
		if err := tree.Unmarshal(&config); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config file %q", configPath)
		}
	}
	return &config, nil
}

func prepareRootDir(dockerCli command.Cli, config *serverConfig) (string, error) {
	rootDir := config.Root
	if rootDir == "" {
		rootDir = rootDataDir(dockerCli)
	}
	if rootDir == "" {
		return "", fmt.Errorf("buildx root dir must be determined")
	}
	if err := os.MkdirAll(rootDir, 0700); err != nil {
		return "", err
	}
	serverRoot := filepath.Join(rootDir, "shared")
	if err := os.MkdirAll(serverRoot, 0700); err != nil {
		return "", err
	}
	return serverRoot, nil
}

func rootDataDir(dockerCli command.Cli) string {
	return filepath.Join(confutil.ConfigDir(dockerCli), "controller")
}

func newBuildxClientAndCheck(addr string, checkNum int, duration time.Duration) (*controller.Client, error) {
	c, err := controller.NewClient(addr)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < checkNum; i++ {
		_, err := c.List(context.TODO())
		if err == nil {
			lastErr = nil
			break
		}
		err = fmt.Errorf("failed to access server (tried %d times): %w", i, err)
		logrus.Debugf("connection failure: %v", err)
		lastErr = err
		time.Sleep(duration)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	p, v, r, err := c.Version(context.TODO())
	if err != nil {
		return nil, err
	}
	logrus.Debugf("connected to server (\"%v %v %v\")", p, v, r)
	if !(p == version.Package && v == version.Version && r == version.Revision) {
		logrus.Warnf("version mismatch (server: \"%v %v %v\", client: \"%v %v %v\"); please kill and restart buildx server",
			p, v, r, version.Package, version.Version, version.Revision)
	}
	return c, nil
}

type buildxController struct {
	*controller.Client
	serverRoot string
}

func (c *buildxController) Kill(ctx context.Context) error {
	pidB, err := os.ReadFile(filepath.Join(c.serverRoot, "pid"))
	if err != nil {
		return err
	}
	pid, err := strconv.ParseInt(string(pidB), 10, 64)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("no PID is recorded for buildx server")
	}
	p, err := os.FindProcess(int(pid))
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		return err
	}
	// TODO: Should we send SIGKILL if process doesn't finish?
	return nil
}

func launch(ctx context.Context, logFile string, args ...string) (func() error, error) {
	bCmd := exec.CommandContext(ctx, os.Args[0], args...)
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		bCmd.Stdout = f
		bCmd.Stderr = f
	}
	bCmd.Stdin = nil
	bCmd.Dir = "/"
	bCmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	if err := bCmd.Start(); err != nil {
		return nil, err
	}
	return bCmd.Wait, nil
}
