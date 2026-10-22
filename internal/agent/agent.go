// Package agent connects a native executor to a Hub without any inbound ports.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

const maxConcurrentRequests = 16

// Run reconnects the transport while retaining the caller-owned executor. It
// never resends a tool request. A disconnected reply is lost, but managed
// processes remain queryable through the executor after reconnection.
func Run(ctx context.Context, cfg config.Agent, executor protocol.Executor, info protocol.Device) error {
	endpoint, err := endpointURL(cfg)
	if err != nil {
		return err
	}
	if executor == nil {
		return errors.New("executor is required")
	}
	info.ID = cfg.DeviceID
	info.Online = true
	info.Version = protocol.Version
	slots := make(chan struct{}, maxConcurrentRequests)
	seen := newRequestIDs()
	var workers sync.WaitGroup
	defer workers.Wait()
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		start := time.Now()
		err = connect(ctx, endpoint, cfg.Token, executor, info, slots, seen, &workers)
		if ctx.Err() != nil {
			return nil
		}
		var authErr *authorizationError
		if errors.As(err, &authErr) {
			return err
		}
		log.Printf("device %s disconnected; reconnecting (%v)", cfg.DeviceID, err)
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		jitter := time.Duration(rand.Int64N(int64(delay/4) + 1))
		timer := time.NewTimer(delay + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func endpointURL(cfg config.Agent) (string, error) {
	if cfg.Version != config.FormatVersion {
		return "", errors.New("unsupported agent configuration version")
	}
	if !config.ValidID(cfg.DeviceID) {
		return "", errors.New("invalid device ID")
	}
	if len(cfg.Token) < 32 || len(cfg.Token) > 1024 {
		return "", errors.New("device token must contain 32-1024 characters")
	}
	u, err := config.CheckURL(cfg.HubURL, cfg.AllowInsecure)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", errors.New("hub_url must use https (or explicitly enabled http)")
	}
	u.Path = "/agent/" + cfg.DeviceID
	return u.String(), nil
}

type authorizationError struct{ code int }

func (e *authorizationError) Error() string {
	return fmt.Sprintf("hub rejected device authentication (HTTP %d); check enrollment or revocation", e.code)
}

func connect(ctx context.Context, endpoint, token string, executor protocol.Executor, info protocol.Device, slots chan struct{}, seen *requestIDs, workers *sync.WaitGroup) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, response, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + token}},
		HTTPClient:      &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		if response != nil && (response.StatusCode == 401 || response.StatusCode == 403) {
			return &authorizationError{code: response.StatusCode}
		}
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxMessageBytes)
	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = wsjson.Write(helloCtx, conn, protocol.Message{Type: "hello", Version: protocol.WireVersion, Device: &info})
	var welcome protocol.Message
	if err == nil {
		err = wsjson.Read(helloCtx, conn, &welcome)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if welcome.Type != "welcome" || welcome.Version != protocol.WireVersion {
		return errors.New("incompatible hub protocol")
	}
	log.Printf("device %s connected", info.ID)
	connectionCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connectionCtx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(connectionCtx, 8*time.Second)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()
	for {
		var msg protocol.Message
		if err := wsjson.Read(connectionCtx, conn, &msg); err != nil {
			return err
		}
		if msg.Type != "request" || msg.ID == "" || len(msg.ID) > 128 || msg.Principal == "" || len(msg.Principal) > 128 || msg.Tool == "" || len(msg.Tool) > 128 {
			return errors.New("invalid hub request")
		}
		if !seen.add(msg.ID) {
			writeError(connectionCtx, conn, msg.ID, "duplicate request was not executed again; query managed process state")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			writeError(connectionCtx, conn, msg.ID, "device is busy: maximum concurrent tool requests reached")
			continue
		}
		workers.Add(1)
		go func(msg protocol.Message) {
			defer workers.Done()
			defer func() { <-slots }()
			// The executor owns OS process lifetimes separately from each Call's wait.
			// Cancel abandoned polls with this connection so they cannot consume later
			// output before a reconnected client can collect it. Never replay the call.
			callCtx, cancel := context.WithTimeout(connectionCtx, 40*time.Second)
			result, err := executor.Call(callCtx, msg.Principal, msg.Tool, msg.Args)
			cancel()
			if err != nil {
				result = protocol.Result{Error: err.Error()}
			}
			reply := protocol.Message{Type: "result", ID: msg.ID, Result: &result}
			encoded, marshalErr := json.Marshal(reply)
			if marshalErr != nil || len(encoded) > protocol.MaxMessageBytes {
				writeError(connectionCtx, conn, msg.ID, "device result exceeds the transport limit; request a smaller output")
				return
			}
			replyCtx, cancel := context.WithTimeout(connectionCtx, 10*time.Second)
			_ = conn.Write(replyCtx, websocket.MessageText, encoded)
			cancel()
		}(msg)
	}
}

func writeError(ctx context.Context, conn *websocket.Conn, id, message string) {
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = wsjson.Write(writeCtx, conn, protocol.Message{Type: "result", ID: id, Result: &protocol.Result{Error: message}})
}

// Store IDs rather than output bodies: a burst of large file reads must not
// turn duplicate suppression into an unbounded result cache on a small VPS.
type requestIDs struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func newRequestIDs() *requestIDs { return &requestIDs{ids: map[string]struct{}{}} }
func (s *requestIDs) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	if len(s.order) >= 2048 {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	return true
}
