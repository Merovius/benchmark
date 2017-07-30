package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/kardianos/osext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robustirc/rafthttp"
	"github.com/robustirc/robustirc/ircserver"
	"github.com/robustirc/robustirc/outputstream"
	"github.com/robustirc/robustirc/raft_store"
	"github.com/robustirc/robustirc/robusthttp"
	"github.com/robustirc/robustirc/types"
	"github.com/stapelberg/glog"
)

const pingInterval = 20 * time.Second

var executablehash = executableHash()

func executableHash() string {
	path, err := osext.Executable()
	if err != nil {
		log.Fatal(err)
	}

	h := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	if _, err := io.Copy(h, f); err != nil {
		log.Fatal(err)
	}

	return fmt.Sprintf("%.16x", h.Sum(nil))
}

// exitOnRecover is used to circumvent the recover handler that net/http
// installs. We need to exit in order to get restarted by the init
// system/supervisor and get into a clean state again.
func exitOnRecover() {
	if r := recover(); r != nil {
		// This mimics go/src/net/http/server.go.
		const size = 64 << 10
		buf := make([]byte, size)
		buf = buf[:runtime.Stack(buf, false)]
		glog.Errorf("http: panic serving: %v\n%s", r, buf)
		glog.Flush()
		os.Exit(1)
	}
}

// HTTP provides an HTTP API to RobustIRC, including HTTP handlers for
// interactive use (e.g. status pages).
type HTTP struct {
	ircServer       *ircserver.IRCServer
	raftNode        *raft.Raft
	peerStore       *raft.JSONPeers
	ircStore        *raft_store.LevelDBStore
	output          *outputstream.OutputStream
	transport       *rafthttp.HTTPTransport
	network         string
	networkPassword string
	raftDir         string
	peerAddr        string
	// getMessagesRequests contains information about each GetMessages
	// request to be exposed on the HTTP status handler.
	getMessagesRequests   map[string]GetMessagesStats
	getMessagesRequestsMu sync.RWMutex
}

func NewHTTP(ircServer *ircserver.IRCServer, raftNode *raft.Raft, peerStore *raft.JSONPeers, ircStore *raft_store.LevelDBStore, output *outputstream.OutputStream, transport *rafthttp.HTTPTransport, network string, networkPassword string, raftDir string, peerAddr string, mux *http.ServeMux) *HTTP {
	api := &HTTP{
		ircServer:           ircServer,
		raftNode:            raftNode,
		peerStore:           peerStore,
		ircStore:            ircStore,
		output:              output,
		transport:           transport,
		network:             network,
		networkPassword:     networkPassword,
		raftDir:             raftDir,
		peerAddr:            peerAddr,
		getMessagesRequests: make(map[string]GetMessagesStats),
	}

	mux.HandleFunc("/robustirc/v1/", api.dispatchPublic)
	mux.HandleFunc("/", api.dispatchPrivate)

	return api
}

var (
	// applyMu guards calls to raft.Apply(). We need to lock them because
	// otherwise we cannot guarantee that multiple goroutines will write
	// strictly monotonically increasing timestamps.
	applyMu sync.Mutex

	// To avoid setting up a new proxy on every request, we cache the proxies
	// for each node (since the current leader might change abruptly).
	nodeProxies   = make(map[string]*httputil.ReverseProxy)
	nodeProxiesMu sync.RWMutex

	// lastContact stores either node.LastContact() for non-leaders or
	// time.Now() for leaders.
	lastContact = time.Now()
)

// GetMessageStats encapsulates information about a GetMessages request.
type GetMessagesStats struct {
	RemoteAddr    string
	Session       types.RobustId
	Nick          string
	Started       time.Time
	UserAgent     string
	ForwardedFor  string
	TrustedBridge string
	cancel        func(superseded bool)
	api           *HTTP
}

func (stats GetMessagesStats) NickWithFallback() string {
	if stats.Nick != "" {
		return stats.Nick
	}
	if session, err := stats.api.ircServer.GetSession(stats.Session); err == nil {
		return session.Nick
	}
	return ""
}

// StartedAndRelative converts |stats.Started| into a human-readable formatted
// time, followed by a relative time specification.
func (stats GetMessagesStats) StartedAndRelative() string {
	return stats.Started.Format("2006-01-02 15:04:05 -07:00") + " (" +
		time.Now().Round(time.Second).Sub(stats.Started.Round(time.Second)).String() + " ago)"
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error {
	return nil
}

func getNodeProxy(leader string) (*httputil.ReverseProxy, bool) {
	nodeProxiesMu.RLock()
	defer nodeProxiesMu.RUnlock()
	p, ok := nodeProxies[leader]
	return p, ok
}

func setNodeProxy(leader string, proxy *httputil.ReverseProxy) {
	nodeProxiesMu.Lock()
	defer nodeProxiesMu.Unlock()
	nodeProxies[leader] = proxy
}

func (api *HTTP) dispatchPrivate(w http.ResponseWriter, r *http.Request) {
	defer exitOnRecover()

	username, password, ok := r.BasicAuth()
	if !ok || username != "robustirc" || password != api.networkPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="robustirc"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		switch r.URL.Path {
		case "/":
			fallthrough
		case "/status":
			api.handleStatus(w, r)
			return

		case "/status/getmessage":
			api.handleStatusGetMessage(w, r)
			return

		case "/status/sessions":
			api.handleStatusSessions(w, r)
			return

		case "/status/irclog":
			api.handleStatusIrclog(w, r)
			return

		case "/status/state":
			api.handleStatusState(w, r)
			return

		case "/irclog":
			api.handleIrclog(w, r)
			return

		case "/snapshot":
			api.handleSnapshot(w, r)
			return

		case "/leader":
			api.handleLeader(w, r)
			return

		case "/config":
			api.handleGetConfig(w, r)
			return

		case "/metrics":
			prometheus.Handler().ServeHTTP(w, r)
			return
		}

	case http.MethodPost:
		if strings.HasPrefix(r.URL.Path, "/raft/") {
			api.transport.ServeHTTP(w, r)
			return
		}

		switch r.URL.Path {
		case "/join":
			api.handleJoin(w, r)
			return

		case "/part":
			api.handlePart(w, r)
			return

		case "/quit":
			api.handleQuit(w, r)
			return

		case "/config":
			api.handlePostConfig(w, r)
			return

		case "/kill":
			api.handleKill(w, r)
			return
		}
	}

	http.Error(w, "Not found", http.StatusNotFound)
}

func (api *HTTP) dispatchPublic(w http.ResponseWriter, r *http.Request) {
	defer exitOnRecover()

	rest := r.URL.Path[len("/robustirc/v1/"):]
	switch r.Method {
	case http.MethodPost:
		if rest == "session" {
			api.handleCreateSession(w, r)
			return
		}

		if strings.HasSuffix(rest, "/message") {
			// Verify there are no slashes in what should be the session ID
			if sessionId := rest[:len(rest)-len("/message")]; strings.Index(sessionId, "/") == -1 {
				if session, err := api.sessionOrProxy(w, r, sessionId); err == nil {
					api.handlePostMessage(w, r, session)
				}
				return
			}
		}

	case http.MethodGet:
		if strings.HasSuffix(rest, "/messages") {
			if sessionId := rest[:len(rest)-len("/messages")]; strings.Index(sessionId, "/") == -1 {
				api.handleGetMessages(w, r, sessionId)
			}
		}

	case http.MethodDelete:
		if sessionId := rest; strings.Index(sessionId, "/") == -1 {
			if session, err := api.sessionOrProxy(w, r, sessionId); err == nil {
				api.handleDeleteSession(w, r, session)
			}
			return
		}
	}

	http.Error(w, "Not found", http.StatusNotFound)
}

// applyMessage applies the specified message to the network via Raft.
func (api *HTTP) applyMessage(msg *types.RobustMessage, timeout time.Duration) (raft.ApplyFuture, error) {
	applyMu.Lock()
	defer applyMu.Unlock()

	msg.Id = api.ircServer.NewRobustMessageId()
	msgbytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	return api.raftNode.Apply(msgbytes, timeout), nil
}

func (api *HTTP) applyMessageWait(msg *types.RobustMessage, timeout time.Duration) error {
	f, err := api.applyMessage(msg, timeout)
	if err != nil {
		return err
	}
	if err := f.Error(); err != nil {
		return err
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
}

// TODO: unexport this, find the correct abstraction layer
func (api *HTTP) ApplyMessageWait(msg *types.RobustMessage, timeout time.Duration) error {
	return api.applyMessageWait(msg, timeout)
}

func (api *HTTP) maybeProxyToLeader(w http.ResponseWriter, r *http.Request, body io.ReadCloser) {
	leader := api.raftNode.Leader()
	if leader == "" {
		http.Error(w, fmt.Sprintf("No leader known. Please try another server."),
			http.StatusInternalServerError)
		return
	}

	p, ok := getNodeProxy(leader)
	if !ok {
		u, err := url.Parse("https://" + leader)
		if err != nil {
			http.Error(w, fmt.Sprintf("url.Parse(): %v", err), http.StatusInternalServerError)
			return
		}
		p = httputil.NewSingleHostReverseProxy(u)
		p.Transport = robusthttp.Transport(true)

		// Races are okay, i.e. overwriting the proxy a different goroutine set up.
		setNodeProxy(leader, p)
	}

	location := *r.URL
	location.Host = leader
	w.Header().Set("Content-Location", location.String())
	log.Printf("Proxying request (%q) to leader %q\n", r.URL.Path, leader)
	r.Body = body
	p.ServeHTTP(w, r)
}

func (api *HTTP) session(r *http.Request, sessionId string) (types.RobustId, error) {
	var sessionid types.RobustId

	id, err := strconv.ParseInt(sessionId, 0, 64)
	if err != nil {
		return sessionid, fmt.Errorf("invalid session: %v", err)
	}

	header := r.Header.Get("X-Session-Auth")
	if header == "" {
		return sessionid, fmt.Errorf("no X-Session-Auth header set")
	}

	auth, err := api.ircServer.GetAuth(types.RobustId{Id: id})
	if err != nil {
		return sessionid, err
	}
	if header != auth {
		return sessionid, fmt.Errorf("invalid X-Session-Auth header")
	}

	sessionid.Id = id

	return sessionid, nil
}

func (api *HTTP) sessionOrProxy(w http.ResponseWriter, r *http.Request, sessionId string) (types.RobustId, error) {
	sessionid, err := api.session(r, sessionId)
	if err == ircserver.ErrSessionNotYetSeen && api.raftNode.State() != raft.Leader {
		// The session might exist on the leader, so we must proxy.
		api.maybeProxyToLeader(w, r, r.Body)
		return sessionid, err
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
	}
	return sessionid, err
}

func (api *HTTP) setGetMessagesRequests(sessionId string, stats GetMessagesStats) {
	api.getMessagesRequestsMu.Lock()
	defer api.getMessagesRequestsMu.Unlock()
	if old, ok := api.getMessagesRequests[sessionId]; ok {
		old.cancel(true)
	}
	api.getMessagesRequests[sessionId] = stats
}

func (api *HTTP) deleteGetMessagesRequests(sessionId string) {
	api.getMessagesRequestsMu.Lock()
	defer api.getMessagesRequestsMu.Unlock()
	delete(api.getMessagesRequests, sessionId)
}

func (api *HTTP) copyGetMessagesRequests() map[string]GetMessagesStats {
	result := make(map[string]GetMessagesStats)
	api.getMessagesRequestsMu.RLock()
	defer api.getMessagesRequestsMu.RUnlock()
	for key, value := range api.getMessagesRequests {
		result[key] = value
	}
	return result
}
