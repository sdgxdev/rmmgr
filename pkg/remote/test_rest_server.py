#!/usr/bin/python
import os
import sys
import BaseHTTPServer
import SocketServer
import SimpleHTTPServer
import signal
import socket
import time


class UnixHTTPServer(BaseHTTPServer.HTTPServer):
    address_family = socket.AF_UNIX

    def server_bind(self):
        SocketServer.TCPServer.server_bind(self)
        self.server_name = "foo"
        self.server_port = 0

    def shutdown(self):
        self.__shutdown_request = True


def log_message(self, format, *args):
    sys.stderr.write("%s - - [%s] %s\n" %(
        self.client_address,
        self.log_date_time_string(), format % args))


Handler = SimpleHTTPServer.SimpleHTTPRequestHandler
Handler.protocol_version = "HTTP/1.1"
Handler.log_message = log_message


bind_addr = "/tmp/.http_server_%d" % os.getpid()
server = UnixHTTPServer(bind_addr, Handler)


def sig_handler(signum, frame):
    print 'handling signal', signum
    global server
    server.shutdown()
    print 'server shutdown'
    sys.exit()


signal.signal(signal.SIGHUP, sig_handler)
if len(sys.argv) > 0:
    wait_time = int(sys.argv[1])
    time.sleep(wait_time)
print "listen on:%s" % bind_addr
server.serve_forever()
