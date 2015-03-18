package chserver

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/jpillora/chisel/share"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

type Server struct {
	*chshare.Logger
	Users       chshare.Users
	fingerprint string
	wsCount     int
	wsServer    websocket.Server
	httpServer  *chshare.HTTPServer
	proxy       *httputil.ReverseProxy
	sshConfig   *ssh.ServerConfig
	sessions    map[string]*chshare.User
}

func NewServer(keySeed, authfile, proxy string) (*Server, error) {
	s := &Server{
		Logger:     chshare.NewLogger("server"),
		wsServer:   websocket.Server{},
		httpServer: chshare.NewHTTPServer(),
		sessions:   map[string]*chshare.User{},
	}
	s.wsServer.Handler = websocket.Handler(s.handleWS)

	//parse users, if provided
	if authfile != "" {
		users, err := chshare.ParseUsers(authfile)
		if err != nil {
			return nil, err
		}
		s.Users = users
	}

	//generate private key (optionally using seed)
	key, _ := chshare.GenerateKey(keySeed)
	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal("Failed to parse key")
	}
	//fingerprint this key
	s.fingerprint = chshare.FingerprintKey(private.PublicKey())
	//create ssh config
	s.sshConfig = &ssh.ServerConfig{
		ServerVersion:    chshare.ProtocolVersion + "-server",
		PasswordCallback: s.authUser,
	}
	s.sshConfig.AddHostKey(private)

	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, s.Errorf("Missing protocol (%s)", u)
		}
		s.proxy = httputil.NewSingleHostReverseProxy(u)
		//always use proxy host
		s.proxy.Director = func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}

	return s, nil
}

func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}
	return s.Wait()
}

func (s *Server) Start(host, port string) error {
	s.Infof("Fingerprint %s", s.fingerprint)
	if len(s.Users) > 0 {
		s.Infof("User authenication enabled")
	}
	if s.proxy != nil {
		s.Infof("Default proxy enabled")
	}
	s.Infof("Listening on %s...", port)

	return s.httpServer.GoListenAndServe(":"+port, http.HandlerFunc(s.handleHTTP))
}

func (s *Server) Wait() error {
	return s.httpServer.Wait()
}

func (s *Server) Close() error {
	//this should cause an error in the open websockets
	return s.httpServer.Close()
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	//websockets upgrade AND has chisel prefix
	if r.Header.Get("Upgrade") == "websocket" &&
		r.Header.Get("Sec-WebSocket-Protocol") == chshare.ProtocolVersion {
		s.wsServer.ServeHTTP(w, r)
		return
	}
	//proxy target was provided
	if s.proxy != nil {
		s.proxy.ServeHTTP(w, r)
		return
	}
	//missing :O
	w.WriteHeader(404)
}

//
func (s *Server) authUser(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	// no auth
	if len(s.Users) == 0 {
		return nil, nil
	}
	// authenticate user
	u, ok := s.Users[c.User()]
	if !ok || u.Pass != string(pass) {
		return nil, errors.New("Invalid auth")
	}
	//insert session
	s.sessions[string(c.SessionID())] = u
	return nil, nil
}

func (s *Server) handleWS(ws *websocket.Conn) {
	// Before use, a handshake must be performed on the incoming net.Conn.
	sshConn, chans, reqs, err := ssh.NewServerConn(ws, s.sshConfig)
	if err != nil {
		s.Debugf("Failed to handshake (%s)", err)
		return
	}

	//load user
	sid := string(sshConn.SessionID())
	var user *chshare.User
	if len(s.Users) > 0 {
		user = s.sessions[sid]
	}

	//verify configuration
	r := <-reqs
	reply := func(err error) {
		r.Reply(err == nil, []byte(err.Error()))
		if err != nil {
			sshConn.Close()
		}
	}
	if r.Type != "config" {
		reply(s.Errorf("expecting config request"))
		return
	}

	c, err := chshare.DecodeConfig(r.Payload)
	if err != nil {
		reply(s.Errorf("invalid config"))
		return
	}

	//if user is provided, ensure they have
	//access to the desired remote
	if user != nil {
		for _, r := range c.Remotes {
			addr := r.RemoteHost + ":" + r.RemotePort
			if !user.HasAccess(addr) {
				reply(s.Errorf("access to '%s' denied", addr))
				return
			}
		}
	}

	//prepare connection logger
	s.wsCount++
	id := s.wsCount
	l := s.Fork("session#%d", id)

	l.Debugf("Open")
	go ssh.DiscardRequests(reqs)
	go chshare.ConnectStreams(l, chans)
	sshConn.Wait()
	l.Debugf("Close")

	if user != nil {
		delete(s.sessions, sid)
	}
}
