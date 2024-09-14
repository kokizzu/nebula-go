/*
 *
 * Copyright (c) 2020 vesoft inc. All rights reserved.
 *
 * This source code is licensed under Apache 2.0 License.
 *
 */

package nebula_go

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/vesoft-inc/fbthrift/thrift/lib/go/thrift"
	"github.com/vesoft-inc/nebula-go/v3/nebula"
	"github.com/vesoft-inc/nebula-go/v3/nebula/graph"
	"golang.org/x/net/http2"
)

type connection struct {
	severAddress HostAddress
	timeout      time.Duration
	returnedAt   time.Time // the connection was created or returned.
	sslConfig    *tls.Config
	useHTTP2     bool
	httpHeader   http.Header
	handshakeKey string
	graph        *graph.GraphServiceClient
}

func newConnection(severAddress HostAddress) *connection {
	return &connection{
		severAddress: severAddress,
		timeout:      0 * time.Millisecond,
		returnedAt:   time.Now(),
		sslConfig:    nil,
		handshakeKey: "",
		graph:        nil,
	}
}

// open opens a transport for the connection
// if sslConfig is not nil, an SSL transport will be created
func (cn *connection) open(hostAddress HostAddress, timeout time.Duration, sslConfig *tls.Config,
	useHTTP2 bool, httpHeader http.Header, handshakeKey string) error {
	ip := hostAddress.Host
	port := hostAddress.Port
	newAdd := net.JoinHostPort(ip, strconv.Itoa(port))
	cn.timeout = timeout
	cn.useHTTP2 = useHTTP2
	cn.handshakeKey = handshakeKey

	var (
		err       error
		transport thrift.Transport
		pf        thrift.ProtocolFactory
	)
	if useHTTP2 {
		if sslConfig != nil {
			transport, err = thrift.NewHTTPPostClientWithOptions("https://"+newAdd, thrift.HTTPClientOptions{
				Client: &http.Client{
					Transport: &http2.Transport{
						TLSClientConfig: sslConfig,
					},
				},
			})
		} else {
			transport, err = thrift.NewHTTPPostClientWithOptions("http://"+newAdd, thrift.HTTPClientOptions{
				Client: &http.Client{
					Transport: &http2.Transport{
						// So http2.Transport doesn't complain the URL scheme isn't 'https'
						AllowHTTP: true,
						// Pretend we are dialing a TLS endpoint. (Note, we ignore the passed tls.Config)
						DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
							_ = cfg
							var d net.Dialer
							return d.DialContext(ctx, network, addr)
						},
					},
				},
			})
		}
		if err != nil {
			return fmt.Errorf("failed to create a net.Conn-backed Transport,: %s", err.Error())
		}
		pf = thrift.NewBinaryProtocolFactoryDefault()
		if httpHeader != nil {
			client, ok := transport.(*thrift.HTTPClient)
			if !ok {
				return fmt.Errorf("failed to get thrift http client")
			}
			for k, vv := range httpHeader {
				if k == "Content-Type" {
					// fbthrift will add "Content-Type" header, so we need to skip it
					continue
				}
				for _, v := range vv {
					// fbthrift set header with http.Header.Add, so we need to set header one by one
					client.SetHeader(k, v)
				}
			}
		}
	} else {
		bufferSize := 128 << 10

		var sock thrift.Transport
		if sslConfig != nil {
			sock, err = thrift.NewSSLSocketTimeout(newAdd, sslConfig, timeout)
		} else {
			sock, err = thrift.NewSocket(thrift.SocketAddr(newAdd), thrift.SocketTimeout(timeout))
		}
		if err != nil {
			return fmt.Errorf("failed to create a net.Conn-backed Transport,: %s", err.Error())
		}
		// Set transport
		bufferedTranFactory := thrift.NewBufferedTransportFactory(bufferSize)
		transport = thrift.NewHeaderTransport(bufferedTranFactory.GetTransport(sock))
		pf = thrift.NewHeaderProtocolFactory()
	}

	cn.graph = graph.NewGraphServiceClientFactory(transport, pf)
	if err = cn.graph.Open(); err != nil {
		return fmt.Errorf("failed to open transport, error: %s", err.Error())
	}
	if !cn.graph.IsOpen() {
		return fmt.Errorf("transport is off")
	}
	return cn.verifyClientVersion()
}

func (cn *connection) verifyClientVersion() error {
	req := graph.NewVerifyClientVersionReq()
	if cn.handshakeKey != "" {
		req.SetVersion([]byte(cn.handshakeKey))
	}
	resp, err := cn.graph.VerifyClientVersion(req)
	if err != nil {
		cn.close()
		return fmt.Errorf("failed to verify client handshakeKey: %s", err.Error())
	}
	if resp.GetErrorCode() != nebula.ErrorCode_SUCCEEDED {
		return fmt.Errorf("incompatible handshakeKey between client and server: %s", string(resp.GetErrorMsg()))
	}
	return nil
}

// reopen reopens the current connection.
// Because the code generated by Fbthrift does not handle the seqID,
// the message will be dislocated when the timeout occurs, resulting in unexpected response.
// When the timeout occurs, the connection will be reopened to avoid the impact of the message.
func (cn *connection) reopen() error {
	cn.close()
	return cn.open(cn.severAddress, cn.timeout, cn.sslConfig, cn.useHTTP2, cn.httpHeader, cn.handshakeKey)
}

// Authenticate
func (cn *connection) authenticate(username, password string) (*graph.AuthResponse, error) {
	resp, err := cn.graph.Authenticate([]byte(username), []byte(password))
	if err != nil {
		err = fmt.Errorf("authentication fails, %s", err.Error())
		if e := cn.graph.Close(); e != nil {
			err = fmt.Errorf("fail to close transport, error: %s", e.Error())
		}
		return nil, err
	}

	return resp, nil
}

func (cn *connection) execute(sessionID int64, stmt string) (*graph.ExecutionResponse, error) {
	return cn.executeWithParameter(sessionID, stmt, map[string]*nebula.Value{})
}

func (cn *connection) executeWithParameter(sessionID int64, stmt string,
	params map[string]*nebula.Value) (*graph.ExecutionResponse, error) {
	resp, err := cn.graph.ExecuteWithParameter(sessionID, []byte(stmt), params)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (cn *connection) executeWithParameterTimeout(sessionID int64, stmt string, params map[string]*nebula.Value, timeoutMs int64) (*graph.ExecutionResponse, error) {
	return cn.graph.ExecuteWithTimeout(sessionID, []byte(stmt), params, timeoutMs)
}

func (cn *connection) executeJson(sessionID int64, stmt string) ([]byte, error) {
	return cn.ExecuteJsonWithParameter(sessionID, stmt, map[string]*nebula.Value{})
}

func (cn *connection) ExecuteJsonWithParameter(sessionID int64, stmt string, params map[string]*nebula.Value) ([]byte, error) {
	jsonResp, err := cn.graph.ExecuteJsonWithParameter(sessionID, []byte(stmt), params)
	if err != nil {
		// reopen the connection if timeout
		_, ok := err.(thrift.TransportException)
		if ok {
			if err.(thrift.TransportException).TypeID() == thrift.TIMED_OUT {
				reopenErr := cn.reopen()
				if reopenErr != nil {
					return nil, reopenErr
				}
				return cn.graph.ExecuteJsonWithParameter(sessionID, []byte(stmt), params)
			}
		}
	}

	return jsonResp, err
}

// Check connection to host address
func (cn *connection) ping() bool {
	_, err := cn.execute(0, "YIELD 1")
	return err == nil
}

// Sign out and release session ID
func (cn *connection) signOut(sessionID int64) error {
	// Release session ID to graphd
	return cn.graph.Signout(sessionID)
}

// Update returnedAt for cleaner
func (cn *connection) release() {
	cn.returnedAt = time.Now()
}

// Close transport
func (cn *connection) close() {
	cn.graph.Close()
}
