package server

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

type ID uint16

var (
	matchip    = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+`) // TODO: make correct
	filtername = regexp.MustCompile(`\W`)                  // non-words
)

type Server struct {
	port          int
	addresses     string
	idPool        <-chan ID
	logf          func(format string, args ...interface{})
	privateKey    ssh.Signer
	newPlayers    chan *Player
	onlinePlayers map[string](*Player)
	lines         int
	Events        chan Event
}

func NewServer(db *Database, port int, idPool <-chan ID) (*Server, error) {
	s := &Server{
		port:          port,
		idPool:        idPool,
		logf:          log.New(os.Stdout, "server: ", 0).Printf,
		onlinePlayers: make(map[string]*Player),
		lines:         1,
		Events:        make(chan Event),
		//newPlayers: make(chan *Player),
	}
	if err := db.GetPrivateKey(s); err != nil {
		return nil, err
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		joins := []string{}
		for _, a := range addrs {
			ipv4 := matchip.FindString(a.String())
			if ipv4 != "" {
				joins = append(joins, fmt.Sprintf(" ssh %s -p %d", ipv4, s.port))
			}
		}
		s.addresses = strings.Join(joins, "\n")
	}
	return s, nil
}

func StartServer(s *Server) {
	// bind to provided port
	server, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: s.port})
	if err != nil {
		log.Fatal(err)
	}
	// accept all tcp
	for {
		tcpConn, err := server.AcceptTCP()
		if err != nil {
			s.logf("accept error (%s)", err)
			continue
		}
		go s.handle(tcpConn)
	}
}

func (s *Server) handle(tcpConn *net.TCPConn) {
	//extract these from connection
	var sshName string
	var hash string
	// perform handshake
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, publicKey ssh.PublicKey) (*ssh.Permissions, error) {
			sshName = conn.User()
			if publicKey != nil {
				m := md5.Sum(publicKey.Marshal())
				hash = hex.EncodeToString(m[:])
			}
			return nil, nil
		},
	}
	config.AddHostKey(s.privateKey)
	sshConn, chans, globalReqs, err := ssh.NewServerConn(tcpConn, config)
	if err != nil {
		s.logf("new connection handshake failed (%s)", err)
		return
	}
	// global requests must be serviced - discard
	go ssh.DiscardRequests(globalReqs)
	// protect against XTR (cross terminal renderering) attacks
	name := filtername.ReplaceAllString(sshName, "")
	// trim name
	maxlen := 100
	if len(name) > maxlen {
		name = string([]rune(name)[:maxlen])
	}
	// get the first channel
	c := <-chans
	// channel requests must be serviced - reject rest
	go func() {
		for c := range chans {
			c.Reject(ssh.Prohibited, "only 1 channel allowed")
		}
	}()
	// must be a 'session'
	if t := c.ChannelType(); t != "session" {
		c.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", t))
		sshConn.Close()
		return
	}
	conn, chanReqs, err := c.Accept()
	if err != nil {
		s.logf("could not accept channel (%s)", err)
		sshConn.Close()
		return
	}
	// non-blocking pull off the id pool
	id := ID(0)
	select {
	case id, _ = <-s.idPool:
	default:
	}
	// show fullgame error
	if id == 0 {
		conn.Write([]byte("This game is full.\r\n"))
		sshConn.Close()
		return
	}
	// default name using id
	if name == "" {
		name = fmt.Sprintf("player-%d", id)
	}
	// if user has no public key for some strange reason, use their ip as their unique id
	if hash == "" {
		if ip, _, err := net.SplitHostPort(tcpConn.RemoteAddr().String()); err == nil {
			hash = ip
		}
	}
	log.Printf("Creating new player %q: id: %d, hash: %s", name, id, hash)
	p := NewPlayer(id, sshName, name, hash, conn)
	s.onlinePlayers[p.Name] = p

	// Start threads
	// Prompt Bar is in beta mode. In futere in this place there will be the GOD thread.
	go God(s)
	go p.receiveActions(s, p)
	go p.resizeWatch()

	go func() {
		for r := range chanReqs {
			ok := false
			log.Printf("[%s] response: %#v", r.Type, r)
			switch r.Type {
			case "shell":
				// We don't accept any commands (Payload),
				// only the default shell.
				if len(r.Payload) == 0 {
					ok = true
				}
			case "pty-req":
				// Responding 'ok' here will let the client
				// know we have a pty ready for input
				ok = true
				strlen := r.Payload[3]
				p.resizes <- parseDims(r.Payload[strlen+4:])
			case "window-change":
				p.resizes <- parseDims(r.Payload)
				continue // no response
			}
			log.Printf("replying ok to a %q request", r.Type)
			r.Reply(ok, nil)
		}
	}()
	s.newPlayers <- p
}

// parseDims extracts two uint32s from the provided buffer.
func parseDims(b []byte) resize {
	if len(b) < 8 {
		return resize{
			width:  0,
			height: 0,
		}
	}
	w := binary.BigEndian.Uint32(b)
	h := binary.BigEndian.Uint32(b[4:])
	return resize{
		width:  w,
		height: h,
	}
}

func fingerprintKey(k ssh.PublicKey) string {
	bytes := md5.Sum(k.Marshal())
	strbytes := make([]string, len(bytes))
	for i, b := range bytes {
		strbytes[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(strbytes, ":")
}
