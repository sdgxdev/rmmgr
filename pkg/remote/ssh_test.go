package remote

import (
	"bytes"
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"golang.org/x/crypto/ssh"

	"github.com/go-resty/resty/v2"
)

type sshPassConfig struct {
	user string
	pass string
	host string
	port int
}

func getSSHPassConfigFromEnv() sshPassConfig {
	port, err := strconv.Atoi(os.Getenv("RMMGR_TEST_PORT"))
	if err != nil {
		panic(fmt.Sprintf("failed to parse test env: RMMGR_TEST_PORT: %s", err))
	}
	return sshPassConfig{
		user: os.Getenv("RMMGR_TEST_USER"),
		pass: os.Getenv("RMMGR_TEST_PASS"),
		host: os.Getenv("RMMGR_TEST_HOST"),
		port: port,
	}
}

var sshPass = getSSHPassConfigFromEnv()

func TestSSHConfig(t *testing.T) {
	opts := sshClientConfigOpts{
		user: sshPass.user,
		password: sshPass.pass,
		host: sshPass.host,
	}

	sshConf, err := buildSSHClientConfig(opts)
	if err != nil {
		t.Errorf("failed to build SSHClientConfig: %s", err)
	}

	hostAndPort := fmt.Sprintf("%s:%d", opts.host, sshPass.port)
	connectFunc := ConnectFunc("tcp", hostAndPort)

	conn, err := connectFunc()
	if err != nil {
		t.Errorf("failed to dial: %s", err)
	}

	sshConn, sshChan, req, err := ssh.NewClientConn(conn, hostAndPort, sshConf)
	if err != nil {
		t.Errorf("failed create ssh conn: %s", err)
	}

	client := ssh.NewClient(sshConn, sshChan, req)

	session, err := client.NewSession()
	if err != nil {
		t.Errorf("failed create ssh session: %s", err)
	}

	err = session.Run("ls /")
	if err != nil {
		t.Errorf("failed run ssh ls: %s", err)
	}

}

func TestSSHCommunicatorNew(t *testing.T) {
	comm, err := NewSSHCommunicator(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	})
	if err != nil {
		t.Errorf("failed to create ssh communicator: %s", err)
	}
	if comm.connInfo.User != sshPass.user {
		t.Errorf("unexpected result user not equal")
	}
}

func TestSSHCommunicatorConnectAndExec(t *testing.T) {
	comm, err := NewSSHCommunicator(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	})
	if err != nil {
		t.Errorf("failed to create ssh communicator: %s", err)
	}
	if comm.connInfo.User != sshPass.user {
		t.Errorf("unexpected result user not equal")
	}

	err = comm.Connect()
	if err != nil {
		t.Errorf("failed to connect: %s", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Errorf("failed to open pipe: %s", err)
	}

	cmd := Cmd{
		Command: "ls -sl /",
		Stdin: os.Stdin,
		Stdout: w,
		Stderr: w,
	}

	err = comm.Exec(&cmd)
	if err != nil {
		t.Errorf("failed to exec cmd: %s", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		reader := bufio.NewReader(r)
		line, _, err := reader.ReadLine()
		if err != nil {
			t.Errorf("failed to read exec: %s", err)
		}
		if !strings.Contains(string(line), "total") {
			t.Errorf("invalid return: %s", line)
		}
		wg.Done()
	}()
	wg.Wait()
	err = comm.Disconnect()
	if err != nil {
		t.Errorf("failed to disconnect: %s", err)
	}
}

func TestSSHCommunicatorUpload(t *testing.T) {
	comm, err := NewSSHCommunicator(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	})
	if err != nil {
		t.Errorf("failed to create ssh communicator: %s", err)
	}
	if comm.connInfo.User != sshPass.user {
		t.Errorf("unexpected result user not equal")
	}

	err = comm.Connect()
	if err != nil {
		t.Errorf("failed to connect: %s", err)
	}

	contentUpload, err := ioutil.ReadFile("ssh_test.go")
	if err != nil {
		t.Errorf("failed to read upload file: %s", err)
	}
	reader := bytes.NewReader(contentUpload)
	err = comm.Upload("ssh_test.go", reader)
	if err != nil {
		t.Errorf("failed to upload file: %s", err)
	}
	err = comm.Disconnect()
	if err != nil {
		t.Errorf("failed to disconnect: %s", err)
	}
}

func TestSSHCommunicatorCreateConn(t *testing.T) {
	comm, err := NewSSHCommunicator(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	})
	if err != nil {
		t.Errorf("failed to create ssh communicator: %s", err)
	}
	if comm.connInfo.User != sshPass.user {
		t.Errorf("unexpected result user not equal")
	}

	err = comm.Connect()
	if err != nil {
		t.Errorf("failed to connect: %s", err)
	}

	contentUpload, err := ioutil.ReadFile("test_rest_server.py")
	if err != nil {
		t.Errorf("failed to read upload file: %s", err)
	}
	reader := bytes.NewReader(contentUpload)
	err = comm.Upload("test_rest_server.py", reader)
	if err != nil {
		t.Errorf("failed to upload file: %s", err)
	}

	// add executable permission
	cmdStdout := new(bytes.Buffer)
	cmdStderr := new(bytes.Buffer)
	cmd := Cmd{
		Command: "chmod +x test_rest_server.py",
		Stdin: os.Stdin,
		Stdout: cmdStdout,
		Stderr: cmdStderr,
	}

	err = comm.Exec(&cmd)
	if err != nil {
		t.Errorf("failed to exec cmd: %s", err)
	}

	err = cmd.Wait()
	if err != nil {
		t.Errorf("failed to add executable permission to test_rest_server: %s", err)
	}

	// run rest api server
	r, w, err := os.Pipe()
	if err != nil {
		t.Errorf("failed to open pipe: %s", err)
	}
	cmdStderr = new(bytes.Buffer)
	cmd = Cmd{
		Command: "./test_rest_server.py 1",
		Stdin: os.Stdin,
		Stdout: w,
		Stderr: cmdStderr,
	}

	err = comm.Exec(&cmd)
	if err != nil {
		t.Errorf("failed to exec cmd: %s", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var bindHost string
	var bindPort string
	go func() {
		reader := bufio.NewReader(r)
		line, _, err := reader.ReadLine()
		if err != nil {
			t.Errorf("failed to read exec: %s", err)
		}
		if !strings.Contains(string(line), "listen on") {
			t.Fatalf("invalid return: %s", line)
		}
		tmpLineRec := strings.Split(strings.TrimRight(string(line), "\n"), ":")
		if len(tmpLineRec) != 3 {
			t.Fatalf("server returns invalid usock addr: %s", string(line))
		}
		bindHost = tmpLineRec[1]
		bindPort = tmpLineRec[2]
		wg.Done()
	}()
	wg.Wait()
	t.Logf("rest api server started remotely and listen on: %s:%s", bindHost, bindPort)

	time.Sleep(time.Second)
	conn, err := comm.CreateConn("tcp", fmt.Sprintf("%s:%s", bindHost, bindPort))
	if err != nil {
		t.Fatalf("failed to create tunnel connection: %s", err)
	}

	transport := http.Transport{
		Dial: func(_, _ string) (net.Conn, error) {
			return conn, nil
		},
	}

	client := resty.New()
	client.SetTransport(&transport).SetScheme("http")
	resp, err := client.R().Get("/")
	if err != nil {
		t.Fatalf("failed to get `index.html` from remote rest api server: %s", err)
	}
	t.Logf("response code: %d", resp.StatusCode())
	t.Logf("response body: %s", string(resp.Body()))

	err = comm.Disconnect()
	if err != nil {
		t.Errorf("failed to disconnect: %s", err)
	}
}