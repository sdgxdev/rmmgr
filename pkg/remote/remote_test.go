package remote

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

func parseAddr(input string) (string, bool) {
	if strings.Contains(input, "listen on") {
		tmpLineRec := strings.Split(input, ":")
		if len(tmpLineRec) != 2 {
			return "", false
		}
		return tmpLineRec[1], true
	}
	return "", false
}

func TestBaseTRPCServiceStartTimeout(t *testing.T) {
	s, err := NewBaseTRPCService(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	},
	"test_rest_server.py",
	"./test_rest_server.py",
	"./",
	"./test_rest_server.py 20",
	3,
	"unix",
	parseAddr,
	)
	if err != nil {
		t.Fatalf("failed to create BaseTRPCService: %s", err)
	}

	_, err = s.PrepareRemote()
	if err == nil {
		t.Fatalf("prepare remote should timeout")
	}
	if !strings.Contains(fmt.Sprintf("%s", err), "timeout") {
		t.Fatalf("error should be timeout")
	}
}

func TestBaseTRPCServiceStartSuccess(t *testing.T) {
	s, err := NewBaseTRPCService(&SSHConnectionInfo{
		User: sshPass.user,
		Password: sshPass.pass,
		Host: sshPass.host,
		Port: sshPass.port,
	},
	"test_rest_server.py",
	"./test_rest_server.py",
	"./",
	"./test_rest_server.py 1",
	3,
	"unix",
	parseAddr,
	)
	if err != nil {
		t.Fatalf("failed to create BaseTRPCService: %s", err)
	}

	addr, err := s.PrepareRemote()
	if err != nil {
		t.Fatalf("failed to prepare remote: %s", err)
	}

	time.Sleep(time.Second)
	conn, err := s.Comm.CreateConn(s.ListenProto, addr)
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

	err = s.Comm.Disconnect()
	if err != nil {
		t.Errorf("failed to disconnect: %s", err)
	}
}
