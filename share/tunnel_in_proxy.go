package chshare

import (
	"context"
	"io"
	"net"

	"github.com/jpillora/sizestr"
	"golang.org/x/crypto/ssh"
)

type GetSSHConn func() ssh.Conn

type Proxy struct {
	*Logger
	ssh    GetSSHConn
	id     int
	count  int
	remote *Remote
	dialer net.Dialer
	tcp    *net.TCPListener
	udp    *udpListener
}

func NewProxy(logger *Logger, ssh GetSSHConn, index int, remote *Remote) (*Proxy, error) {
	id := index + 1
	p := &Proxy{
		Logger: logger.Fork("proxy#%d: %s", id, remote),
		ssh:    ssh,
		id:     id,
		remote: remote,
	}
	return p, p.listen()
}

func (p *Proxy) listen() error {
	if p.remote.Stdio {
		//TODO check if pipes active?
	} else if p.remote.LocalProto == "tcp" {
		addr, err := net.ResolveTCPAddr("tcp", p.remote.LocalHost+":"+p.remote.LocalPort)
		if err != nil {
			return p.Errorf("resolve: %s", err)
		}
		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			return p.Errorf("tcp: %s", err)
		}
		p.Infof("Listening")
		p.tcp = l
	} else if p.remote.LocalProto == "udp" {
		l, err := bindSSHUDP(p.Logger, p.ssh, p.remote)
		if err != nil {
			return err
		}
		p.Infof("Listening")
		p.udp = l
	} else {
		return p.Errorf("unknown local proto")
	}
	return nil
}

//Run enables the proxy and blocks while its active,
//close the proxy by cancelling the context.
func (p *Proxy) Run(ctx context.Context) error {
	if p.remote.Stdio {
		p.runStdio(ctx)
	} else if p.remote.LocalProto == "tcp" {
		return p.runTCP(ctx)
	} else if p.remote.LocalProto == "udp" {
		//udp doesnt accept connections,
		//udp simply forwards all packets
		//and therefore only needs to listen
		return p.udp.run(ctx)
	}
	panic("should not get here")
}

func (p *Proxy) runStdio(ctx context.Context) {
	for {
		p.pipeRemote(Stdio)
		select {
		case <-ctx.Done():
			return
		default:
			// the connection is not ready yet, keep waiting
		}
	}
}

func (p *Proxy) runTCP(ctx context.Context) error {
	done := make(chan struct{})
	//implements missing net.ListenContext
	go func() {
		select {
		case <-ctx.Done():
			p.tcp.Close()
			p.Infof("Closed")
		case <-done:
		}
	}()
	for {
		src, err := p.tcp.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				//listener closed
			default:
				p.Infof("Accept error: %s", err)
			}
			close(done)
			return err
		}
		go p.pipeRemote(src)
	}
}

func (p *Proxy) pipeRemote(src io.ReadWriteCloser) {
	defer src.Close()
	p.count++
	cid := p.count
	l := p.Fork("conn#%d", cid)
	l.Debugf("Open")
	sshConn := p.ssh()
	if sshConn == nil {
		l.Debugf("No remote connection")
		return
	}
	//ssh request for tcp connection for this proxy's remote
	dst, reqs, err := sshConn.OpenChannel("chisel", []byte(p.remote.Remote()))
	if err != nil {
		l.Infof("Stream error: %s", err)
		return
	}
	go ssh.DiscardRequests(reqs)
	//then pipe
	s, r := Pipe(src, dst)
	l.Debugf("Close (sent %s received %s)", sizestr.ToString(s), sizestr.ToString(r))
}

//TCPProxy makes this package backward compatible
type TCPProxy = Proxy

//NewTCPProxy makes this package backward compatible
var NewTCPProxy = NewProxy
