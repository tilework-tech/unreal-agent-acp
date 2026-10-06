// Command fake-model serves a canned OpenAI Responses API for running the ACP
// conformance suite against the real binary without a model provider. It
// prints its base URL on stdout. Prompts containing the cancel sentinel never
// complete, so cancellation always lands while a turn is in flight.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
)

const cancelSentinel = "TCK-CANCEL"

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-model:", err)
		os.Exit(1)
	}
	_, handler := fakemodel.New(func(request fakemodel.Request) fakemodel.Reply {
		if slices.ContainsFunc(request.UserTexts(), func(text string) bool { return strings.Contains(text, cancelSentinel) }) {
			<-request.Context().Done()
			return fakemodel.Reply{}
		}
		return fakemodel.Text("ok")
	})
	fmt.Printf("http://%s\n", listener.Addr())
	if err := http.Serve(listener, handler); err != nil {
		fmt.Fprintln(os.Stderr, "fake-model:", err)
		os.Exit(1)
	}
}
