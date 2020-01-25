package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	// randShared is a global random generator object that is shared.  This must be
	// shared since it is seeded by the current time and creating multiple can
	// result in the same values. By using a shared RNG we assure different numbers
	// per call.
	randLock   sync.Mutex
	randShared *rand.Rand

	// enable ssh keeplive probes by default
	keepAliveInterval = 2 * time.Second

	// max time to wait for for a KeepAlive response before considering the
	// connection to be dead.
	maxKeepAliveDelay = 120 * time.Second
)

// SSHCommunicator is the ssh implementation of the Communicator interface.
type SSHCommunicator struct {
	connInfo *SSHConnectionInfo
	client   *ssh.Client
	config   *sshConfig
	conn     net.Conn
	cancelKeepAlive context.CancelFunc
	lock sync.Mutex
}

// NewSSHCommunicator create a SSHCommunicator
func NewSSHCommunicator(connInfo *SSHConnectionInfo) (*SSHCommunicator, error) {
	host := fmt.Sprintf("%s:%d", connInfo.Host, connInfo.Port)
	sshConf, err := buildSSHClientConfig(sshClientConfigOpts{
		user: connInfo.User,
		host: host,
		privateKey: connInfo.PrivateKey,
		password: connInfo.Password,
		hostKey: connInfo.HostKey,
		certificate: connInfo.Certificate,
	})
	if err != nil {
		return nil, err
	}

	connectFunc := ConnectFunc("tcp", host)

	if connInfo.BastionHost != "" {
		bastionHost := fmt.Sprintf("%s:%d", connInfo.BastionHost, connInfo.BastionPort)
		bastionConf, err := buildSSHClientConfig(sshClientConfigOpts{
			user:        connInfo.BastionUser,
			host:        bastionHost,
			privateKey:  connInfo.BastionPrivateKey,
			password:    connInfo.BastionPassword,
			hostKey:     connInfo.HostKey,
			certificate: connInfo.BastionCertificate,
		})
		if err != nil {
			return nil, err
		}

		connectFunc = BastionConnectFunc("tcp", bastionHost, bastionConf, "tcp", host)
	}

	config := &sshConfig{
		config:     sshConf,
		connection: connectFunc,
	}

	// Setup the random number generator once. The seed value is the
	// time multiplied by the PID. This can overflow the int64 but that
	// is okay. We multiply by the PID in case we have multiple processes
	// grabbing this at the same time. This is possible with Terraform and
	// if we communicate to the same host at the same instance, we could
	// overwrite the same files. Multiplying by the PID prevents this.
	randLock.Lock()
	defer randLock.Unlock()
	if randShared == nil {
		randShared = rand.New(rand.NewSource(
			time.Now().UnixNano() * int64(os.Getpid())))
	}

	comm := &SSHCommunicator{
		connInfo: connInfo,
		config: config,
	}

	return comm, nil
}

// Connect implementation of communicator.Communicator interface
func (c *SSHCommunicator) Connect() (err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.conn != nil {
		c.conn.Close()
	}

	c.conn  = nil
	c.client = nil

	hostAndPort := fmt.Sprintf("%s:%d", c.connInfo.Host, c.connInfo.Port)
	log.Printf("[DEBUG] Connecting to %s for SSH", hostAndPort)
	c.conn, err = c.config.connection()
	if err != nil {
		c.conn = nil
		log.Printf("[ERROR] connection error: %s", err)
		return err
	}

	log.Printf("[DEBUG] Connection established. Handshaking for user %v", c.connInfo.User)
	sshConn, sshChan, req, err := ssh.NewClientConn(c.conn, hostAndPort, c.config.config)
	if err != nil {
		err = fmt.Errorf("SSH authentication failed (%s@%s): %s", c.connInfo.User, hostAndPort, err)
		log.Printf("[WARN] %s", err)
		return err
	}

	c.client = ssh.NewClient(sshConn, sshChan, req)

	if err != nil {
		return err
	}

	log.Printf("[Info] connected!")

	ctx, cancelKeepAlive := context.WithCancel(context.TODO())
	c.cancelKeepAlive = cancelKeepAlive

	// Start a keepalive goroutine to help maintain the connection for
	// long-running commands.
	log.Printf("[DEBUG] starting ssh KeepAlives")
	go func() {
		defer cancelKeepAlive()
		// Along with the KeepAlives generating packets to keep the tcp
		// connection open, we will use the replies to verify liveness of the
		// connection. This will prevent dead connections from blocking the
		// provisioner indefinitely.
		respCh := make(chan error, 1)

		go func() {
			t := time.NewTicker(keepAliveInterval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					_, _, err := c.client.SendRequest("keepalive@ssh.communicator", true, nil)
					respCh <- err
				case <-ctx.Done():
					return
				}
			}
		}()

		after := time.NewTimer(maxKeepAliveDelay)
		defer after.Stop()

		for {
			select {
			case err := <-respCh:
				if err != nil {
					log.Printf("[ERROR] ssh keepalive: %s", err)
					sshConn.Close()
					return
				}
			case <-after.C:
				// abort after too many missed keepalives
				log.Println("[ERROR] no reply from ssh server")
				sshConn.Close()
				return
			case <-ctx.Done():
				return
			}
			if !after.Stop() {
				<-after.C
			}
			after.Reset(maxKeepAliveDelay)
		}
	}()

	return nil
}

// Disconnect implementation of communicator.Communicator interface
func (c *SSHCommunicator) Disconnect() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.cancelKeepAlive != nil {
		c.cancelKeepAlive()
	}

	if c.conn != nil {
		conn := c.conn
		c.conn = nil
		return conn.Close()
	}

	return nil
}

// Exec implements the Communicator.Exec interface
func (c *SSHCommunicator) Exec(cmd *Cmd) error {
		cmd.Init()

	session, err := c.newSession()
	if err != nil {
		return err
	}

	// The sending signal function is added. However, old openssh-server 
	// doesn't support handling signal from client channel.
	cmd.sendSigFunc = func(sig string) error {
		log.Printf("[DEBUG] signal sending to remote: %s\n", sig)
		return session.Signal(ssh.Signal(sig))
	}

	// Setup our session
	session.Stdin = cmd.Stdin
	session.Stdout = cmd.Stdout
	session.Stderr = cmd.Stderr

	if !c.config.noPty {
		// Request a PTY
		termModes := ssh.TerminalModes{
			ssh.ECHO:          0,     // do not echo
			ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
			ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
		}

		if err := session.RequestPty("xterm", 80, 40, termModes); err != nil {
			return err
		}
	}
	log.Printf("[DEBUG] starting remote command: %s", cmd.Command)
	err = session.Start(strings.TrimSpace(cmd.Command) + "\n")
	if err != nil {
		return err
	}

	// Start a goroutine to wait for the session to end and set the
	// exit boolean and status.
	go func() {
		defer session.Close()

		err := session.Wait()
		exitStatus := 0
		if err != nil {
			exitErr, ok := err.(*ssh.ExitError)
			if ok {
				exitStatus = exitErr.ExitStatus()
			}
		}

		cmd.SetExitStatus(exitStatus, err)
		log.Printf("[DEBUG] remote command exited with '%d': %s", exitStatus, cmd.Command)
	}()

	return nil
}

func (c *SSHCommunicator) newSession() (session *ssh.Session, err error) {
	log.Println("[DEBUG] opening new ssh session")
	if c.client == nil {
		err = errors.New("ssh client is not connected")
	} else {
		session, err = c.client.NewSession()
	}

	if err != nil {
		log.Printf("[WARN] ssh session open error: '%s', attempting reconnect", err)
		if err := c.Connect(); err != nil {
			return nil, err
		}

		return c.client.NewSession()
	}

	return session, nil
}

// Upload implements Communicator.Upload interface
func (c *SSHCommunicator) Upload(path string, input io.Reader) error {
		// The target directory and file for talking the SCP protocol
	targetDir := filepath.Dir(path)
	targetFile := filepath.Base(path)

	// On windows, filepath.Dir uses backslash separators (ie. "\tmp").
	// This does not work when the target host is unix.  Switch to forward slash
	// which works for unix and windows
	targetDir = filepath.ToSlash(targetDir)

	// Skip copying if we can get the file size directly from common io.Readers
	size := int64(0)

	switch src := input.(type) {
	case *os.File:
		fi, err := src.Stat()
		if err != nil {
			size = fi.Size()
		}
	case *bytes.Buffer:
		size = int64(src.Len())
	case *bytes.Reader:
		size = int64(src.Len())
	case *strings.Reader:
		size = int64(src.Len())
	}

	scpFunc := func(w io.Writer, stdoutR *bufio.Reader) error {
		return scpUploadFile(targetFile, input, w, stdoutR, size)
	}

	return c.scpSession("scp -vt "+targetDir, scpFunc)
}

func scpUploadFile(dst string, src io.Reader, w io.Writer, r *bufio.Reader, size int64) error {
	if size == 0 {
		return fmt.Errorf("upload file(%s) size must be provided", dst)
	}

	// Start the protocol
	log.Println("[DEBUG] Beginning file upload...")
	fmt.Fprintln(w, "C0644", size, dst)
	if err := checkSCPStatus(r); err != nil {
		return err
	}

	if _, err := io.Copy(w, src); err != nil {
		return err
	}

	fmt.Fprint(w, "\x00")
	if err := checkSCPStatus(r); err != nil {
		return err
	}

	return nil
}

// checkSCPStatus checks that a prior command sent to SCP completed
// successfully. If it did not complete successfully, an error will
// be returned.
func checkSCPStatus(r *bufio.Reader) error {
	code, err := r.ReadByte()
	if err != nil {
		return err
	}

	if code != 0 {
		// Treat any non-zero (really 1 and 2) as fatal errors
		message, _, err := r.ReadLine()
		if err != nil {
			return fmt.Errorf("Error reading error message: %s", err)
		}

		return errors.New(string(message))
	}

	return nil
}

func (c *SSHCommunicator) scpSession(scpCommand string, f func(io.Writer, *bufio.Reader) error) error {
	session, err := c.newSession()
	if err != nil {
		return err
	}
	defer session.Close()

	// Get a pipe to stdin so that we can send data down
	stdinW, err := session.StdinPipe()
	if err != nil {
		return err
	}

	// We only want to close once, so we nil w after we close it,
	// and only close in the defer if it hasn't been closed already.
	defer func() {
		if stdinW != nil {
			stdinW.Close()
		}
	}()

	// Get a pipe to stdout so that we can get responses back
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stdoutR := bufio.NewReader(stdoutPipe)

	// Set stderr to a bytes buffer
	stderr := new(bytes.Buffer)
	session.Stderr = stderr

	// Start the sink mode on the other side
	log.Println("[DEBUG] Starting remote scp process: ", scpCommand)
	if err := session.Start(scpCommand); err != nil {
		return err
	}

	// Call our callback that executes in the context of SCP. We ignore
	// EOF errors if they occur because it usually means that SCP prematurely
	// ended on the other side.
	log.Println("[DEBUG] Started SCP session, beginning transfers...")
	if err := f(stdinW, stdoutR); err != nil && err != io.EOF {
		return err
	}

	// Close the stdin, which sends an EOF, and then set w to nil so that
	// our defer func doesn't close it again since that is unsafe with
	// the Go SSH package.
	log.Println("[DEBUG] SCP session complete, closing stdin pipe.")
	stdinW.Close()
	stdinW = nil

	// Wait for the SCP connection to close, meaning it has consumed all
	// our data and has completed. Or has errored.
	log.Println("[DEBUG] Waiting for SSH session to complete.")
	err = session.Wait()

	// log any stderr before exiting on an error
	scpErr := stderr.String()
	if len(scpErr) > 0 {
		log.Printf("[ERROR] scp stderr: %q", stderr)
	}

	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			// Otherwise, we have an ExitErorr, meaning we can just read
			// the exit status
			log.Printf("[ERROR] %s", exitErr)

			// If we exited with status 127, it means SCP isn't available.
			// Return a more descriptive error for that.
			if exitErr.ExitStatus() == 127 {
				return errors.New(
					"SCP failed to start. This usually means that SCP is not\n" +
						"properly installed on the remote system.")
			}
		}

		return err
	}

	return nil
}

// CreateConn implements the Communicator.CreateConn interface.
func (c *SSHCommunicator) CreateConn(proto, addr string) (net.Conn, error) {
	log.Println("[DEBUG] opening new ssh session")
	if c.client == nil {
		return nil, errors.New("ssh client is not connected")
	} 
	return c.client.Dial(proto, addr)

}