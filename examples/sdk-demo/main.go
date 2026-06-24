// Command sdk-demo shows driving a metis agent out-of-process with the
// pkg/client SDK: spawn `metis acp`, send one prompt, stream the reply.
//
//	go run ./examples/sdk-demo "summarize go's error handling in one line"
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Ricardo-M-L/metis/pkg/client"
)

func main() {
	ctx := context.Background()

	// Spawn the agent in bypass mode so the demo runs unattended. A real
	// app would omit this and handle permission_request events instead.
	c, err := client.Spawn(ctx, client.WithArgs("--mode", "bypass"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(1)
	}
	defer c.Close()
	fmt.Fprintf(os.Stderr, "connected to %s %s (acp %s)\n", c.Server.Name, c.Server.Version, c.ProtocolVersion)

	prompt := "Reply with exactly: SDK-DEMO-OK"
	if len(os.Args) > 1 {
		prompt = os.Args[1]
	}

	err = c.Prompt(ctx, prompt, func(ev client.Event) {
		switch ev.Kind {
		case "text_delta":
			fmt.Print(ev.TextDelta)
		case "tool_start":
			fmt.Fprintf(os.Stderr, "\n[tool: %s]\n", ev.ToolName)
		}
	})
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "prompt:", err)
		os.Exit(1)
	}
}
