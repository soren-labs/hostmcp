// Package client provides a small SDK-based CLI client and stdio bridge.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

type authTransport struct {
	base   http.RoundTripper
	token  string
	origin string
}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != t.origin {
		return nil, errors.New("refusing to send credential to a different origin")
	}
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// A frame may appear as both structured content and escaped text in MCP.
	const maxResponse = 16 << 20
	if resp.ContentLength > maxResponse {
		_ = resp.Body.Close()
		return nil, errors.New("MCP response exceeds 16 MiB")
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: maxResponse}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, errors.New("MCP response exceeds 16 MiB")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func Token(path string) (string, error) {
	if path == "" {
		token := strings.TrimSpace(os.Getenv("HOSTMCP_TOKEN"))
		if len(token) < 32 || len(token) > 1024 || strings.ContainsAny(token, " \r\n\t") {
			return "", errors.New("provide --token-file or HOSTMCP_TOKEN")
		}
		return token, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > 1024 || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("token file must be a regular, owner-only file of at most 1024 bytes (chmod 600)")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 32 || len(token) > 1024 || strings.ContainsAny(token, " \r\n\t") {
		return "", errors.New("invalid bearer credential")
	}
	return token, nil
}

func Endpoint(raw string, allowInsecure bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/mcp" {
		return "", errors.New("MCP URL path must be /mcp")
	}
	u.Path = ""
	if _, err = config.CheckURL(u.String(), allowInsecure); err != nil {
		return "", err
	}
	u.Path = "/mcp"
	return u.String(), nil
}

func HTTPClient(endpoint, token string) *http.Client {
	u, _ := url.Parse(endpoint)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns = 8
	base.MaxIdleConnsPerHost = 4
	base.MaxConnsPerHost = 16
	base.ResponseHeaderTimeout = 40 * time.Second
	return &http.Client{Transport: authTransport{base: base, token: token, origin: u.Scheme + "://" + u.Host}, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect refused; configure the final MCP URL")
	}}
}

func Connect(ctx context.Context, raw, token, protocolVersion string, allowInsecure bool) (*mcp.ClientSession, error) {
	endpoint, err := Endpoint(raw, allowInsecure)
	if err != nil {
		return nil, err
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "hostmcp-client", Version: protocol.Version}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: HTTPClient(endpoint, token), DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: protocol.MaxMessageBytes}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
}

// Bridge exposes the remote Hub tools to any stdio-capable MCP client. It never
// executes a remote tool locally and never retries a failed side-effecting call.
func Bridge(ctx context.Context, session *mcp.ClientSession) error {
	s := mcp.NewServer(&mcp.Implementation{Name: "hostmcp-connect", Version: protocol.Version}, nil)
	count := 0
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("list remote tools: %w", err)
		}
		count++
		if count > 256 {
			return errors.New("remote tool catalog exceeds limit")
		}
		copyTool := *tool
		s.AddTool(&copyTool, func(callctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out, err := session.CallTool(callctx, &mcp.CallToolParams{Name: copyTool.Name, Arguments: r.Params.Arguments})
			if err != nil {
				result := &mcp.CallToolResult{}
				result.SetError(fmt.Errorf("remote call failed; outcome may be unknown; inspect list_processes before retrying: %w", err))
				return result, nil
			}
			return out, nil
		})
	}
	return s.Run(ctx, &mcp.StdioTransport{MaxLineLength: protocol.MaxMessageBytes})
}
