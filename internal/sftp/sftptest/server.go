// Package sftptest runs an in-process sshd serving the SFTP subsystem, so the
// code that talks to a server can be tested against a server.
//
// A fake would not be worth much: what there is to get wrong in reaching a
// remote machine — the handshake, the host key callback, subsystem
// negotiation, how a dropped connection surfaces — has no existence above the
// protocol, and a stand-in for the protocol would agree with whatever the code
// under test believed. So the tests speak the protocol, and the only thing
// pretended at is the filesystem behind it, which is a temp directory.
package sftptest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Server is an in-process sshd listening on a real TCP socket on localhost.
type Server struct {
	// Addr is the host:port it is listening on.
	Addr string

	// Fingerprint is its host key, in the "SHA256:…" form ssh-keygen prints.
	Fingerprint string

	// Authorized is the one client public key the server will accept. Nil —
	// the default — accepts any key, which is what a test that is not about
	// authentication wants.
	Authorized ssh.PublicKey

	// Password is accepted for password sign-in. Empty accepts none.
	Password string

	// NoSubsystem makes the server refuse the sftp subsystem, for the test
	// that a server with SFTP switched off is reported as such rather than as
	// an unexplained EOF.
	NoSubsystem bool

	hostKey  ssh.Signer
	listener net.Listener
	wg       sync.WaitGroup
}

// NewServer starts a server rooted at root and stops it when the test ends.
func NewServer(t testing.TB, root string) *Server {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &Server{
		Addr:        listener.Addr().String(),
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		hostKey:     signer,
		listener:    listener,
	}

	s.wg.Add(1)
	go s.serve(root)

	t.Cleanup(func() {
		listener.Close()
		s.wg.Wait()
	})
	return s
}

// HostPort splits the listening address into the two fields a client config
// wants.
func (s *Server) HostPort(t testing.TB) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", s.Addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q in %q is not a number: %v", port, s.Addr, err)
	}
	return host, n
}

func (s *Server) serve(root string) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn, root)
		}()
	}
}

func (s *Server) handle(conn net.Conn, root string) {
	defer conn.Close()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.Authorized == nil {
				return &ssh.Permissions{}, nil
			}
			if string(key.Marshal()) == string(s.Authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown public key")
		},
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if s.Password != "" && string(password) == s.Password {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("wrong password")
		},
	}
	cfg.AddHostKey(s.hostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "only sessions here")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.session(channel, requests, root)
	}
}

func (s *Server) session(channel ssh.Channel, requests <-chan *ssh.Request, root string) {
	for req := range requests {
		if req.Type != "subsystem" || s.NoSubsystem {
			req.Reply(false, nil)
			continue
		}
		// The payload is a length-prefixed string; "sftp" is the only one
		// worth answering.
		if len(req.Payload) < 4 || string(req.Payload[4:]) != "sftp" {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)

		server, err := pkgsftp.NewServer(channel, pkgsftp.WithServerWorkingDirectory(root))
		if err != nil {
			channel.Close()
			return
		}
		if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
			channel.Close()
			return
		}
		server.Close()
		channel.Close()
		return
	}
}

// NewClientKey returns a fresh ed25519 key as unencrypted OpenSSH PEM, with
// its public half for a Server's Authorized field.
func NewClientKey(t testing.TB) (privatePEM string, public ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "sand-test")
	if err != nil {
		t.Fatalf("marshalling client key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("client public key: %v", err)
	}
	return string(pem.EncodeToMemory(block)), sshPub
}

// NewEncryptedClientKey returns a key sealed under a passphrase.
func NewEncryptedClientKey(t testing.TB, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "sand-test", []byte(passphrase))
	if err != nil {
		t.Fatalf("sealing client key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}
