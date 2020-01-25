package remote

import (
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHConnectionInfo is used to construct a SSH communicator
type SSHConnectionInfo struct {
	User        string
	Password    string
	PrivateKey  string 
	Certificate string 
	Host        string
	HostKey     string 
	Port        int
	Timeout     string
	ScriptPath  string
	TimeoutVal  time.Duration

	BastionUser        string
	BastionPassword    string
	BastionPrivateKey  string
	BastionCertificate string
	BastionHost        string
	BastionHostKey     string
	BastionPort        int
}

type sshClientConfigOpts struct {
	privateKey  string
	password    string
	certificate string
	user        string
	host        string
	hostKey     string
}

type sshConfig struct {
	config *ssh.ClientConfig
	connection func() (net.Conn, error)
	noPty bool
}

func buildSSHClientConfig(opts sshClientConfigOpts) (*ssh.ClientConfig, error) {
	hkCallback := ssh.InsecureIgnoreHostKey()

	//TODO: implement host key check
	
	conf := &ssh.ClientConfig{
		HostKeyCallback: hkCallback,
		User: opts.user,
	}

	if opts.privateKey != "" {
		if opts.certificate != "" {
			certSigner, err := signCertWithPrivateKey(opts.privateKey, opts.certificate)
			if err != nil {
				return nil, err
			}
			conf.Auth = append(conf.Auth, certSigner)
		} else {
			pubKeyAuth, err := readPrivateKey(opts.privateKey)
			if err != nil {
				return nil, err
			}
			conf.Auth = append(conf.Auth, pubKeyAuth)
		}
	}

	if opts.password != "" {
		conf.Auth = append(conf.Auth, ssh.Password(opts.password))
	}

	//TODO: implement sshAgent

	return conf, nil
}

// ConnectFunc returns a function returning tcp connection 
func ConnectFunc(proto, addr string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		c, err := net.DialTimeout(proto, addr, 15*time.Second)
		if err != nil {
			return nil, err
		}
		if tcpConn, ok := c.(*net.TCPConn); ok {
			tcpConn.SetKeepAlive(true)
		}

		return c, nil
	}
}

// BastionConnectFunc is a convenience method for returning a function
// that connects to a host over a bastion connection.
func BastionConnectFunc(
	bProto string,
	bAddr string,
	bConf *ssh.ClientConfig,
	proto string,
	addr string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		log.Printf("[DEBUG] Connecting to bastion: %s", bAddr)
		bastion, err := ssh.Dial(bProto, bAddr, bConf)
		if err != nil {
			return nil, fmt.Errorf("Error connecting to bastion: %s", err)
		}

		log.Printf("[DEBUG] Connecting via bastion (%s) to host: %s", bAddr, addr)
		conn, err := bastion.Dial(proto, addr)
		if err != nil {
			bastion.Close()
			return nil, err
		}

		// Wrap it up so we close both things properly
		return &bastionConn{
			Conn:    conn,
			Bastion: bastion,
		}, nil
	}
}

type bastionConn struct {
	net.Conn
	Bastion *ssh.Client
}

func (c *bastionConn) Close() error {
	c.Conn.Close()
	return c.Bastion.Close()
}

// Create a Cert Signer and return ssh.AuthMethod
func signCertWithPrivateKey(pk string, certificate string) (ssh.AuthMethod, error) {
	rawPk, err := ssh.ParseRawPrivateKey([]byte(pk))
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key %q: %s", pk, err)
	}

	pcert, _, _, _, err := ssh.ParseAuthorizedKey([]byte(certificate))
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate %q: %s", certificate, err)
	}

	usigner, err := ssh.NewSignerFromKey(rawPk)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer from raw private key %q: %s", rawPk, err)
	}

	ucertSigner, err := ssh.NewCertSigner(pcert.(*ssh.Certificate), usigner)
	if err != nil {
		return nil, fmt.Errorf("failed to create cert signer %q: %s", usigner, err)
	}

	return ssh.PublicKeys(ucertSigner), nil
}

func readPrivateKey(pk string) (ssh.AuthMethod, error) {
	// We parse the private key on our own first so that we can
	// show a nicer error if the private key has a password.
	block, _ := pem.Decode([]byte(pk))
	if block == nil {
		return nil, errors.New("Failed to read ssh private key: no key found")
	}
	if block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		return nil, errors.New(
			"Failed to read ssh private key: password protected keys are\n" +
				"not supported. Please decrypt the key prior to use.")
	}

	signer, err := ssh.ParsePrivateKey([]byte(pk))
	if err != nil {
		return nil, fmt.Errorf("Failed to parse ssh private key: %s", err)
	}

	return ssh.PublicKeys(signer), nil
}
