package remote

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TRPCService is an interface defining behaviors of a tunneled rpc service.
type TRPCService interface {
	// Start is used to start remote service
	Start() error

	// Stop is used to stop remote service
	Stop() error

	// GetClient returns 
	GetClient() *rpc.Client
}

// BaseTRPCService implements common behaviors of TRPCServices
type BaseTRPCService struct {
	lock                 sync.Mutex
	Comm                 *SSHCommunicator
	ServiceBinName       string
	ServiceBinPath       string
	ServiceRunDir        string
	ServiceCommand       string
	ServiceStartWait     int
	ListenProto          string
	ListenAddrParseFunc	 func(string) (string, bool)
}

// NewBaseTRPCService create a BaseTRPCService instance
func NewBaseTRPCService(
	connInfo *SSHConnectionInfo,
	serviceBinName string,
	serviceBinPath string,
	serviceRunDir  string,
	serviceCommand string,
	serviceStartWait int,
	lproto string,
	laddrParseFunc func(string) (string, bool),
) (*BaseTRPCService, error) {
	comm, err := NewSSHCommunicator(connInfo)
	if err != nil {
		return nil, err
	}
	return &BaseTRPCService{
		lock: sync.Mutex{},
		Comm: comm,
		ServiceBinName: serviceBinName,
		ServiceBinPath: serviceBinPath,
		ServiceRunDir:  serviceRunDir,
		ServiceCommand: serviceCommand,
		ServiceStartWait: serviceStartWait,
		ListenProto: lproto,
		ListenAddrParseFunc: laddrParseFunc,
	}, nil
}

// PrepareRemote connect ssh and run service on remote end.
func (s *BaseTRPCService) PrepareRemote() (string, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	sshTargetAddr := fmt.Sprintf("%s:%d", s.Comm.connInfo.Host, s.Comm.connInfo.Port)
	log.Printf("[DEBUG] Preparing remote service on %s", sshTargetAddr)
	err := s.Comm.Connect()
	if err != nil {
		log.Printf("[ERROR] Failed connect to SSH(%s).", sshTargetAddr)
		return "", err
	}
	log.Printf("[DEBUG] SSH(%s) connected.", sshTargetAddr)

	log.Printf("[DEBUG] Copying service binary to %s", sshTargetAddr)
	serviceBin, err := ioutil.ReadFile(s.ServiceBinPath)
	if err != nil {
		log.Printf("[ERROR] Failed to read service binary: %s", s.ServiceBinPath)
		return "", err
	}
	reader := bytes.NewReader(serviceBin)
	err = s.Comm.Upload(filepath.Join(s.ServiceRunDir, s.ServiceBinName), reader)
	if err != nil {
		log.Printf("[ERROR] Failed to upload bin %s to %s:%s", s.ServiceBinPath, sshTargetAddr, s.ServiceRunDir)
		return "", err
	} 
	log.Printf("[DEBUG] Service binary copied to %s", sshTargetAddr)

	log.Printf("[DEBUG] Adding executable permission to %s on %s", s.ServiceBinName, sshTargetAddr)
	// add executable permission
	cmdStdout := new(bytes.Buffer)
	cmdStderr := new(bytes.Buffer)
	cmd := Cmd{
		Command: "chmod +x slurmagent",
		Stdin: os.Stdin,
		Stdout: cmdStdout,
		Stderr: cmdStderr,
	}
	err = s.Comm.Exec(&cmd)
	if err != nil {
		log.Printf("[ERROR] Failed to add executable permission to %s on %s", s.ServiceBinName, sshTargetAddr)
		return "", err
	}
	err = cmd.Wait()
	if err != nil {
		log.Printf("[ERROR] Failed to add executable permission to %s on %s", s.ServiceBinName, sshTargetAddr)
		return "", err
	}
	log.Printf("[DEBUG] Added executable permission to %s on %s", s.ServiceBinName, sshTargetAddr)

	r, w, err := os.Pipe()
	if err != nil {
		log.Printf("[ERROR] Failed to open pipe: %s", err)
		return "", err
	}
	cmdStderr = new(bytes.Buffer)
	cmd = Cmd{
		Command: s.ServiceCommand,
		Stdin: os.Stdin,
		Stdout: w,
		Stderr: cmdStderr,
	}
	err = s.Comm.Exec(&cmd)
	if err != nil {
		log.Printf("[ERROR] Failed to run service %s on %s", s.ServiceBinName, sshTargetAddr)
		return "", err
	}

	log.Printf("[DEBUG] Running service(%s) on %s", s.ServiceBinName, sshTargetAddr)
	addrChan := make(chan string)
	go func() {
		reader := bufio.NewReader(r)
		parsed := false
		for {
			line, _, err := reader.ReadLine()
			if err != nil {
				log.Printf(
					"[ERROR] Failed to read remote(%s) service(%s) output: %s",
					err,
					sshTargetAddr,
					s.ServiceBinName,
				)
				if err == io.EOF {
					break
				}
			}
			lineStr := strings.TrimRight(string(line), "\n")
			log.Printf("[Remote(%s)]%s", sshTargetAddr, lineStr)
			if !parsed {
				if addr, ok := s.ListenAddrParseFunc(lineStr); ok {
					addrChan <- addr
				}
			}
		}
	}()

	timer := time.NewTimer(time.Second*time.Duration(s.ServiceStartWait))
	select {
	case addr := <- addrChan:
		log.Printf("[DEBUG] Got remote(%s) listen addr: %s", sshTargetAddr, addr)
		return addr, nil
	case <- timer.C:
		log.Printf("[DEBUG] Got remote(%s) listen addr timeout", sshTargetAddr)
		return "", fmt.Errorf("start remote service timeout after %d seconds", s.ServiceStartWait)
	}
}

// Cmd represents a remote command is going to be run remotely.
type Cmd struct {
	Command string
	Stdin io.Reader
	Stdout io.Writer
	Stderr io.Writer
	exitStatus int
	exitCh chan struct{}
	err error
	sync.Mutex
	sendSigFunc func(string) error
}

// Init must be called by the Communicator before executing the command.
func (c *Cmd) Init() {
	c.Lock()
	defer c.Unlock()

	c.exitCh = make(chan struct{})
}

// SetExitStatus stores the exit status of the remote command as well as any
// communicator related error. SetExitStatus then unblocks any pending calls
// to Wait.
// This should only be called by communicators executing the remote.Cmd.
func (c *Cmd) SetExitStatus(status int, err error) {
	c.Lock()
	defer c.Unlock()

	c.exitStatus = status
	c.err = err

	close(c.exitCh)
}

// Wait waits for the remote command to complete.
// Wait may return an error from the communicator, or an ExitError if the
// process exits with a non-zero exit status.
func (c *Cmd) Wait() error {
	<-c.exitCh

	c.Lock()
	defer c.Unlock()

	if c.err != nil || c.exitStatus != 0 {
		return &ExitError{
			Command:    c.Command,
			ExitStatus: c.exitStatus,
			Err:        c.err,
		}
	}

	return nil
}

// SendSignal sends signal to the remote command process.
func (c *Cmd) SendSignal(sig string) error {
	if c.sendSigFunc != nil {
		return c.sendSigFunc(sig)
	}
	return fmt.Errorf("sendSigFunc not set yet")
}

// ExitError is returned by Wait to indicate and error executing the remote
// command, or a non-zero exit status.
type ExitError struct {
	Command    string
	ExitStatus int
	Err        error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("error executing %q: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("%q exit status: %d", e.Command, e.ExitStatus)
}

// Communicator is a wrapper of ssh client and provides high level functions.
type Communicator interface {

	// Connect is used to setup the connection
	Connect() error

	// Disconnect is used to terminate the connection
	Disconnect() error

	// Upload is used to upload a single file
	Upload(string, io.Reader) error

	// Exec is used to execute a command remotely in a new session
	Exec(*Cmd) error

	// CreateConn is used to create a connection to a remote tcp port over a ssh channel
	CreateConn(string, string) (net.Conn, error)
}