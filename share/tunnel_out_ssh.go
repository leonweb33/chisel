package chshare

import (
	"io"
	"net"
	"strings"

	"github.com/jpillora/sizestr"
	"golang.org/x/crypto/ssh"
)

func (t *Tunnel) handleSSHRequests(reqs <-chan *ssh.Request) {
	for r := range reqs {
		switch r.Type {
		case "ping":
			r.Reply(true, nil)
		default:
			t.Debugf("Unknown request: %s", r.Type)
		}
	}
}

func (t *Tunnel) handleSSHChannels(chans <-chan ssh.NewChannel) {
	for ch := range chans {
		go t.handleSSHChannel(ch)
	}
}

func (t *Tunnel) handleSSHChannel(ch ssh.NewChannel) {
	remote := string(ch.ExtraData())
	udp := remote == "udp"
	socks := remote == "socks"
	if socks && t.socksServer == nil {
		t.Debugf("Denied socks request, please enable socks")
		ch.Reject(ssh.Prohibited, "SOCKS5 is not enabled")
		return
	}
	stream, reqs, err := ch.Accept()
	if err != nil {
		t.Debugf("Failed to accept stream: %s", err)
		return
	}
	defer stream.Close()
	go ssh.DiscardRequests(reqs)
	l := t.Logger.Fork("conn#%d", t.connStats.New())
	//ready to handle
	t.connStats.Open()
	l.Debugf("Open %s", t.connStats.String())
	if socks {
		err = t.handleSocks(stream)
	} else if udp {
		err = t.handleUDP(l, stream)
	} else {
		err = t.handleTCP(l, stream, remote)
	}
	t.connStats.Close()
	if err != nil && !strings.HasSuffix(err.Error(), "EOF") {
		l.Debugf("Close %s (error %s)", t.connStats.String(), err)
	} else {
		l.Debugf("Close %s", t.connStats.String())
	}
}

func (t *Tunnel) handleSocks(src io.ReadWriteCloser) error {
	return t.socksServer.ServeConn(NewRWCConn(src))
}

func (t *Tunnel) handleTCP(l *Logger, src io.ReadWriteCloser, remote string) error {
	dst, err := net.Dial("tcp", remote)
	if err != nil {
		return err
	}
	s, r := Pipe(src, dst)
	l.Debugf("sent %s received %s", sizestr.ToString(s), sizestr.ToString(r))
	return nil
}
