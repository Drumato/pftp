package pftp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

const (
	BUFFER_SIZE = 4096
)

type proxyServer struct {
	id             int
	timeout        int
	clientReader   *bufio.Reader
	clientWriter   *bufio.Writer
	originReader   *bufio.Reader
	originWriter   *bufio.Writer
	origin         net.Conn
	passThrough    bool
	mutex          *sync.Mutex
	log            *logger
	sem            int
	proxyProtocol  bool
	stopChan       chan struct{}
	stop           bool
	secureCommands []string
}

type proxyServerConfig struct {
	timeout        int
	clientReader   *bufio.Reader
	clientWriter   *bufio.Writer
	originAddr     string
	mutex          *sync.Mutex
	log            *logger
	proxyProtocol  bool
	secureCommands []string
}

func newProxyServer(conf *proxyServerConfig) (*proxyServer, error) {
	c, err := net.Dial("tcp", conf.originAddr)
	if err != nil {
		return nil, err
	}

	p := &proxyServer{
		clientReader:   conf.clientReader,
		clientWriter:   conf.clientWriter,
		originWriter:   bufio.NewWriter(c),
		originReader:   bufio.NewReader(c),
		origin:         c,
		timeout:        conf.timeout,
		passThrough:    true,
		mutex:          conf.mutex,
		log:            conf.log,
		proxyProtocol:  conf.proxyProtocol,
		stopChan:       make(chan struct{}),
		secureCommands: conf.secureCommands,
	}
	p.log.debug("new proxy from=%s to=%s", c.LocalAddr(), c.RemoteAddr())

	return p, err
}

func (s *proxyServer) sendToOrigin(line string) error {
	cnt := 0
	if s.timeout > 0 {
		s.origin.SetReadDeadline(time.Now().Add(time.Duration(time.Second.Nanoseconds() * int64(s.timeout))))
	}

	for {
		if cnt > s.timeout {
			return errors.New("Could not get semaphore to send to client")
		}

		s.commandLog(line)

		if s.semFree() {
			if _, err := s.origin.Write([]byte(line)); err != nil {
				return err
			}
			s.semLock()
			break
		}
		time.Sleep(1 * time.Second)
		cnt++
	}
	return nil
}

func (s *proxyServer) responseProxy() error {
	return s.start(s.originReader, s.clientWriter)
}

func (s *proxyServer) semLock() {
	s.sem++
}

func (s *proxyServer) semUnlock() {
	s.sem--
}

func (s *proxyServer) semFree() bool {
	return s.sem < 1
}

func (s *proxyServer) semLocked() bool {
	return s.sem > 0
}

func (s *proxyServer) suspend() error {
	s.log.debug("suspend proxy")
	cnt := 0
	for {
		if cnt > s.timeout {
			return errors.New("Could not get semaphore to send to client")
		}

		if s.semFree() {
			s.passThrough = false
			break
		}
		time.Sleep(1 * time.Second)
		cnt++
	}
	return nil
}

func (s *proxyServer) unsuspend() {
	s.log.debug("unsuspend proxy")
	s.passThrough = true
}

func (s *proxyServer) Close() {
	s.origin.Close()
}

func sendProxyHeader(conn net.Conn, clientAddr string, originAddr string) error {
	sourceAddr := strings.Split(clientAddr, ":")
	destinationAddr := strings.Split(originAddr, ":")
	sourcePort, _ := strconv.Atoi(sourceAddr[1])
	destinationPort, _ := strconv.Atoi(destinationAddr[1])

	// proxyProtocolHeader's DestinationAddress must be IP! not domain name
	hostIP, err := net.LookupIP(destinationAddr[0])
	if err != err {
		return err
	}

	proxyProtocolHeader := proxyproto.Header{
		Version:            byte(1),
		Command:            proxyproto.PROXY,
		TransportProtocol:  proxyproto.TCPv4,
		SourceAddress:      net.ParseIP(sourceAddr[0]),
		DestinationAddress: net.ParseIP(hostIP[0].String()),
		SourcePort:         uint16(sourcePort),
		DestinationPort:    uint16(destinationPort),
	}

	_, err = proxyProtocolHeader.WriteTo(conn)
	return err
}

/* send command before login to origin.                  *
*  TLS version set by client to pftp tls version         *
*  because client/pftp/origin must set same TLS version. */
func (s *proxyServer) sendTLSCommand(tlsProtocol uint16, previousTLSCommands []string, newConn net.Conn, reader *bufio.Reader, writer *bufio.Writer) error {
	for _, cmd := range previousTLSCommands {
		s.commandLog(cmd)
		if _, err := newConn.Write([]byte(cmd)); err != nil {
			return fmt.Errorf("failed to make TLS connection")
		}

		// read response from new origin server
		str, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to make TLS connection")
		}

		if strings.Compare(strings.ToUpper(strings.SplitN(strings.Trim(cmd, "\r\n"), " ", 2)[0]), "AUTH") == 0 {
			code := strings.SplitN(strings.Trim(str, "\r\n"), " ", 2)[0]
			if code[0] == '5' {
				return fmt.Errorf("origin server has not support TLS connection")
			}

			config := tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tlsProtocol,
				MaxVersion:         tlsProtocol,
			}

			// SSL/TLS wrapping on connection
			newConn = tls.Client(newConn, &config)
			reader = bufio.NewReader(newConn)
			writer = bufio.NewWriter(newConn)
		}
	}

	s.origin = newConn
	s.originReader = reader
	s.originWriter = writer

	return nil
}

func (s *proxyServer) switchOrigin(clientAddr string, originAddr string, tlsProtocol uint16, previousTLSCommands []string) error {
	s.log.info("switch origin to: %s", originAddr)

	if s.passThrough {
		err := s.suspend()
		if err != nil {
			return err
		}
		defer s.unsuspend()
	}

	s.stopChan <- struct{}{}

	c, err := net.Dial("tcp", originAddr)
	if err != nil {
		s.stop = false
		return err
	}
	reader := bufio.NewReader(c)
	writer := bufio.NewWriter(c)

	old := s.origin

	// Send proxy protocol v1 header when set proxy protocol true
	if s.proxyProtocol {
		if err := sendProxyHeader(c, clientAddr, originAddr); err != nil {
			return err
		}
	}

	// read welcome message
	if _, err := reader.ReadString('\n'); err != nil {
		return err
	}

	// If client connect with TLS connection, make TLS connection to origin ftp server too.
	// If cannot make TLS connection with origin, use old to origin for quit process.
	if err := s.sendTLSCommand(tlsProtocol, previousTLSCommands, c, reader, writer); err != nil {
		c.Close()

		s.stop = false
		return err
	}

	old.Close()

	s.stop = false
	return nil
}

func (s *proxyServer) start(from *bufio.Reader, to *bufio.Writer) error {
	if s.stop {
		return nil
	}

	buff := make([]byte, BUFFER_SIZE)
	read := make(chan []byte, BUFFER_SIZE)
	done := make(chan struct{})
	send := make(chan struct{})
	errchan := make(chan error)
	var lastError error

	go func() {
		for {
			n, err := from.Read(buff)
			s.log.debug("response from server: %s", buff[:n])
			if err != nil {
				if err != io.EOF {
					safeSetChanel(errchan, err)
				} else {
					// when receive EOF from origin, send EOF to client for terminate session completely
					if s.passThrough || s.semLocked() {
						read <- buff[:n]
						if s.semLocked() {
							s.semUnlock()
						}
					}
					<-send

					lastError = err
					s.stopChan <- struct{}{}
				}
				break
			} else {
				if s.timeout > 0 {
					s.origin.SetReadDeadline(time.Now().Add(time.Duration(time.Second.Nanoseconds() * int64(s.timeout))))
				}

				if s.passThrough || s.semLocked() {
					read <- buff[:n]
					if s.semLocked() {
						s.semUnlock()
					}
				}

				<-send
			}
		}
		done <- struct{}{}
	}()

loop:
	for {
		select {
		case b, ok := <-read:
			if !ok {
				break loop
			}

			s.mutex.Lock()
			_, err := to.Write(b)
			if err != nil {
				lastError = err
				s.mutex.Unlock()
				break loop
			}

			if err := to.Flush(); err != nil {
				lastError = err
				s.mutex.Unlock()
				break loop
			}
			s.mutex.Unlock()
			send <- struct{}{}
		case err := <-errchan:
			lastError = err
			break loop
		case <-s.stopChan:
			close(errchan)
			// close read groutine
			s.origin.Close()
			s.stop = true
			break loop
		}
	}
	close(read)
	<-done

	return lastError
}

// Hide parameters from log
func (s *proxyServer) commandLog(line string) {
	command := strings.ToUpper(strings.SplitN(strings.Trim(line, "\r\n"), " ", 2)[0])
	hideParams := false
	for _, c := range s.secureCommands {
		if strings.Compare(command, c) == 0 {
			hideParams = true
			break
		}
	}

	if hideParams {
		s.log.debug("send to origin: %s ********", command)
	} else {
		s.log.debug("send to origin: %s", line)
	}
}
