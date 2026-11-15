// Command fakechild is a stdio MCP server for the childmcp tests. It stands
// in for glovo-skill.
//
// Tools:
//   - echo: returns its "text" argument, plus structured content.
//   - tool_error: returns isError true.
//   - rpc_error: answers with a JSON-RPC error.
//   - crash: exits with status 3 without answering.
//   - slow: answers after FAKE_SLOW (default 500ms).
//   - add_tool: registers "late_tool" and emits tools/list_changed.
//   - glovo_get_basket: ok, or isError when FAKE_BASKET=fail, or no
//     content when FAKE_BASKET=empty.
//   - pid: returns the process id.
//   - glovo_checkout, glovo_place_order: must never be mirrored.
//
// FAKE_EXIT_AT_START=1 makes it exit before serving.
package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	if os.Getenv("FAKE_EXIT_AT_START") == "1" {
		os.Exit(4)
	}
	s := server.NewMCPServer("fake-glovo", "0.0.1", server.WithToolCapabilities(true))
	text := func(t string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: t}}}
	}
	s.AddTool(mcp.NewTool("echo", mcp.WithDescription("echo text"), mcp.WithString("text", mcp.Required())),
		func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res := text(r.GetString("text", ""))
			res.StructuredContent = map[string]any{"echo": r.GetString("text", ""), "env": os.Getenv("FAKE_MARK")}
			return res, nil
		})
	s.AddTool(mcp.NewTool("tool_error"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := text("upstream said no")
		res.IsError = true
		return res, nil
	})
	s.AddTool(mcp.NewTool("rpc_error"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("upstream rpc failure")
	})
	s.AddTool(mcp.NewTool("crash"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		os.Exit(3)
		return nil, nil
	})
	s.AddTool(mcp.NewTool("slow"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		d, err := time.ParseDuration(os.Getenv("FAKE_SLOW"))
		if err != nil {
			d = 500 * time.Millisecond
		}
		time.Sleep(d)
		return text("slow done"), nil
	})
	s.AddTool(mcp.NewTool("add_tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.AddTool(mcp.NewTool("late_tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text("late"), nil
		})
		return text("added"), nil
	})
	s.AddTool(mcp.NewTool("glovo_get_basket"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		switch os.Getenv("FAKE_BASKET") {
		case "fail":
			res := text("Glovo auth failed (401). Run glovo_login first.")
			res.IsError = true
			return res, nil
		case "empty":
			return &mcp.CallToolResult{Content: []mcp.Content{}}, nil
		}
		return text(`{"baskets":[]}`), nil
	})
	s.AddTool(mcp.NewTool("pid"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return text(strconv.Itoa(os.Getpid())), nil
	})
	for _, n := range []string{"glovo_checkout", "glovo_place_order"} {
		s.AddTool(mcp.NewTool(n), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text("ORDER PLACED"), nil
		})
	}
	if err := server.ServeStdio(s); err != nil {
		os.Exit(1)
	}
}
