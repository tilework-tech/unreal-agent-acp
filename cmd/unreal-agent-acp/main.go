// Command unreal-agent-acp is an Agent Client Protocol agent over stdio that
// runs unreal-agent in-process. stdout carries only ACP frames; logs go to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/tilework-tech/unreal-agent-acp/internal/acpagent"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

const (
	sessionDirectoryEnvironment = "UNREAL_AGENT_ACP_SESSION_DIRECTORY"
	modelsEnvironment           = "UNREAL_AGENT_ACP_MODELS"
)

// version is set at release time with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	flags := flag.NewFlagSet("unreal-agent-acp", flag.ContinueOnError)
	printVersion := flags.Bool("version", false, "print the version and exit")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: unreal-agent-acp [--version]")
		fmt.Fprintln(flags.Output(), "Speaks the Agent Client Protocol over stdin/stdout. Configuration is read from the environment.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unreal-agent-acp: unexpected arguments %q\n", flags.Args())
		flags.Usage()
		os.Exit(2)
	}
	if *printVersion {
		fmt.Println("unreal-agent-acp", agentVersion())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := acpagent.Serve(ctx, os.Stdin, os.Stdout, acpagent.Config{
		Name:    "unreal-agent-acp",
		Version: agentVersion(),
		Runtime: unrealrt.Config{SessionDirectory: os.Getenv(sessionDirectoryEnvironment)},
		Models:  models(os.Getenv(modelsEnvironment)),
		Logger:  logger,
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "unreal-agent-acp:", err)
		os.Exit(1)
	}
}

func models(list string) []string {
	var names []string
	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// agentVersion is the release version, else the module version recorded by
// the Go toolchain (for example by go install), else "dev".
func agentVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
