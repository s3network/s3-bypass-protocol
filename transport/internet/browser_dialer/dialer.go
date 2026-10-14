package browser_dialer

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	goerrors "errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/uuid"
)

//go:embed dialer.html
var webpage []byte

type task struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	Extra          any    `json:"extra,omitempty"`
	StreamResponse bool   `json:"streamResponse"`
}

const (
	// connsBufferSize is how many browser connections are kept around.
	connsBufferSize = 256
	// maxDialAttempts bounds how many dead browser connections a single dial
	// may skip before giving up, so it can never spin forever.
	maxDialAttempts = 8
	// readHeaderTimeout protects the browser dialer http server against
	// slowloris style connections that never finish sending headers.
	readHeaderTimeout = 10 * time.Second
)

// dialerState is the whole mutable state of the browser dialer. Reload()
// replaces it atomically, so a dial happening concurrently either uses the old
// state or the new one, never a half initialized one.
type dialerState struct {
	addr   string
	conns  chan *websocket.Conn
	server *http.Server
}

var (
	current atomic.Pointer[dialerState]
	mu      sync.Mutex
)

var upgrader = &websocket.Upgrader{
	ReadBufferSize:   0,
	WriteBufferSize:  0,
	HandshakeTimeout: time.Second * 4,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Used by external projects when using xray as a go module
func Reload() {
	addr := platform.NewEnvFlag(platform.BrowserDialerAddress).GetValue(func() string { return "" })

	mu.Lock()
	defer mu.Unlock()

	old := current.Load()
	if old != nil && old.addr == addr {
		return
	}
	if old != nil {
		closeDialer(old)
	}

	if addr == "" {
		current.Store(nil)
		return
	}

	token := uuid.New()
	csrfToken := token.String()
	page := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte(csrfToken))

	conns := make(chan *websocket.Conn, connsBufferSize)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/websocket" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			_, _ = w.Write(page)
			return
		}
		if r.URL.Query().Get("token") != csrfToken {
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errors.LogErrorInner(r.Context(), err, "browser dialer websocket upgrade failed")
			return
		}
		// Never block here: if no dialer is waiting for a connection, drop it
		// instead of parking the http handler on a full buffer forever.
		select {
		case conns <- conn:
		default:
			conn.Close()
		}
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	current.Store(&dialerState{
		addr:   addr,
		conns:  conns,
		server: server,
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && !goerrors.Is(err, http.ErrServerClosed) {
			errors.LogErrorInner(context.Background(), err, "browser dialer stopped listening on ", addr)
		}
	}()
}

// closeDialer stops the http server and closes every pooled browser connection.
func closeDialer(s *dialerState) {
	if s.server != nil {
		_ = s.server.Close()
	}
	if s.conns == nil {
		return
	}
	for {
		select {
		case c := <-s.conns:
			c.Close()
		default:
			return
		}
	}
}

func HasBrowserDialer() bool {
	s := current.Load()
	return s != nil && s.conns != nil
}

type webSocketExtra struct {
	Protocol string `json:"protocol,omitempty"`
}

func DialWS(ctx context.Context, uri string, ed []byte) (*websocket.Conn, error) {
	task := task{
		Method:         "WS",
		URL:            uri,
		StreamResponse: true,
	}

	task.Extra = webSocketExtra{
		Protocol: base64.RawURLEncoding.EncodeToString(ed),
	}

	return dialTask(ctx, task)
}

type httpExtra struct {
	Referrer string            `json:"referrer,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Cookies  map[string]string `json:"cookies,omitempty"`
}

func httpExtraFromHeadersAndCookies(headers http.Header, cookies []*http.Cookie) *httpExtra {
	if len(headers) == 0 {
		return nil
	}

	extra := httpExtra{}
	if referrer := headers.Get("Referer"); referrer != "" {
		extra.Referrer = referrer
		headers.Del("Referer")
	}

	if len(headers) > 0 {
		extra.Headers = make(map[string]string)
		for header := range headers {
			extra.Headers[header] = headers.Get(header)
		}
	}

	if len(cookies) > 0 {
		extra.Cookies = make(map[string]string)
		for _, cookie := range cookies {
			extra.Cookies[cookie.Name] = cookie.Value
		}
	}

	return &extra
}

func DialGet(ctx context.Context, uri string, headers http.Header, cookies []*http.Cookie) (*websocket.Conn, error) {
	task := task{
		Method:         "GET",
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: true,
	}

	return dialTask(ctx, task)
}

func DialPacket(ctx context.Context, method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	return dialWithBody(ctx, method, uri, headers, cookies, payload)
}

func dialWithBody(ctx context.Context, method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	task := task{
		Method:         method,
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: false,
	}

	conn, err := dialTask(ctx, task)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return err
	}

	return CheckOK(conn)
}

func dialTask(ctx context.Context, task task) (*websocket.Conn, error) {
	s := current.Load()
	if s == nil || s.conns == nil {
		return nil, errors.New("browser dialer is not enabled")
	}

	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}

	var conn *websocket.Conn
	for range maxDialAttempts {
		// Waiting for a browser connection must never outlive the dial itself.
		select {
		case c, ok := <-s.conns:
			if !ok {
				return nil, errors.New("browser dialer is closed")
			}
			conn = c
		case <-ctx.Done():
			return nil, errors.New("failed to get a browser connection").Base(ctx.Err())
		}

		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			conn.Close()
			conn = nil
			continue
		}
		break
	}
	if conn == nil {
		return nil, errors.New("no usable browser connection")
	}

	if err := CheckOK(conn); err != nil {
		return nil, err
	}

	return conn, nil
}

func CheckOK(conn *websocket.Conn) error {
	if _, p, err := conn.ReadMessage(); err != nil {
		conn.Close()
		return err
	} else if s := string(p); s != "ok" {
		conn.Close()
		return errors.New(s)
	}

	return nil
}

func init() {
	platform.RegisterEnvReload(func() error {
		Reload()
		return nil
	})
}
