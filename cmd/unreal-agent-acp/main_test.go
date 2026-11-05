package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
)

func TestMain(m *testing.M) {
	if os.Getenv("UNREAL_AGENT_ACP_TEST_RUN_MAIN") == "1" {
		os.Args = os.Args[:1] // drop the test binary's own flags
		main()
		return
	}
	os.Exit(m.Run())
}

// TestStdioSessionRoundTrip runs the real binary entrypoint as a subprocess and
// checks that stdout carries only JSON-RPC frames.
func TestStdioSessionRoundTrip(t *testing.T) {
	model := fakemodel.Start(t, fakemodel.Sequence(fakemodel.Text("hello over stdio")))
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), "UNREAL_AGENT_ACP_TEST_RUN_MAIN=1", "UNREAL_AGENT_ACP_SESSION_DIRECTORY="+t.TempDir())
	for name, value := range model.Env() {
		command.Env = append(command.Env, name+"="+value)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	send := func(line string) {
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	// next returns the next frame, failing on anything that is not JSON-RPC.
	next := func() map[string]json.RawMessage {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed; stderr:\n%s", stderr.String())
			}
			var frame map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &frame); err != nil || string(frame["jsonrpc"]) != `"2.0"` {
				t.Fatalf("non-JSON-RPC output on stdout: %q", line)
			}
			return frame
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out; stderr:\n%s", stderr.String())
			return nil
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if frame := next(); frame["result"] == nil {
		t.Fatalf("initialize = %v", frame)
	}
	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q,"mcpServers":[]}}`, t.TempDir()))
	var created struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	frame := next()
	encoded, _ := json.Marshal(frame)
	json.Unmarshal(encoded, &created)
	if created.Result.SessionID == "" {
		t.Fatalf("session/new = %v", frame)
	}
	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"hi"}]}}`, created.Result.SessionID))
	var sawText bool
	for {
		frame := next()
		if string(frame["method"]) == `"session/update"` && strings.Contains(string(frame["params"]), "hello over stdio") {
			sawText = true
		}
		if string(frame["id"]) == "3" {
			if !strings.Contains(string(frame["result"]), `"end_turn"`) {
				t.Fatalf("prompt response = %v", frame)
			}
			break
		}
	}
	if !sawText {
		t.Fatal("agent message was not streamed")
	}

	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("agent exited with %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		command.Process.Kill()
		t.Fatal("agent did not exit when stdin closed")
	}
}

// buildAgent compiles the command with the given -ldflags, as a release build does.
func buildAgent(t *testing.T, ldflags string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "unreal-agent-acp")
	build := exec.Command("go", "build", "-ldflags", ldflags, "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return binary
}

func TestVersionFlagPrintsTheReleaseVersion(t *testing.T) {
	binary := buildAgent(t, "-X main.version=1.2.3")
	for _, flag := range []string{"--version", "-version"} {
		output, err := exec.Command(binary, flag).Output()
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if got := strings.TrimSpace(string(output)); got != "unreal-agent-acp 1.2.3" {
			t.Fatalf("%s printed %q", flag, got)
		}
	}
}

func TestInitializeReportsTheReleaseVersion(t *testing.T) {
	binary := buildAgent(t, "-X main.version=1.2.3")
	command := exec.Command(binary)
	command.Env = append(os.Environ(), "UNREAL_AGENT_ACP_SESSION_DIRECTORY="+t.TempDir())
	command.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			AgentInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"agentInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("initialize output %q: %v", output, err)
	}
	if response.Result.AgentInfo.Name != "unreal-agent-acp" || response.Result.AgentInfo.Version != "1.2.3" {
		t.Fatalf("agentInfo = %+v", response.Result.AgentInfo)
	}
}

func TestUnknownArgumentsAreRejected(t *testing.T) {
	binary := buildAgent(t, "")
	command := exec.Command(binary, "--no-such-flag")
	command.Stdin = strings.NewReader("")
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("err = %v, want exit status 2", err)
	}
}
