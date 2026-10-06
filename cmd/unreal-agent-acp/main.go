// Command unreal-agent-acp is an Agent Client Protocol agent over stdio that
// runs unreal-agent in-process. stdout carries only ACP frames; logs go to stderr.
package main

import (
	"context"
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := acpagent.Serve(ctx, os.Stdin, os.Stdout, acpagent.Config{
		Name:    "unreal-agent-acp",
		Version: version(),
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

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
