/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remotecommand

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	spdy "k8s.io/client-go/transport/spdy"
)

// StreamOptions holds information pertaining to the current streaming session: supported stream
// protocols, input/output streams, if the client is requesting a TTY, and a terminal size queue to
// support terminal resizing.
// StreamOptions存储了和当前streaming session有关的信息
type StreamOptions struct {
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	Tty               bool
	TerminalSizeQueue TerminalSizeQueue
}

// Executor is an interface for transporting shell-style streams.
// Executor是用于传输shell风格的流
type Executor interface {
	// Stream initiates the transport of the standard shell streams. It will transport any
	// non-nil stream to a remote system, and return an error if a problem occurs. If tty
	// is set, the stderr stream is not used (raw TTY manages stdout and stderr over the
	// stdout stream).
	// Stream初始化标准shell模式的流传输，它会将non-nil stream传往远程系统，并且在遇到问题时返回error
	// 如果设置了tty，就不会使用stderr stream（raw TTY会通过stdout stream管理stdout和stderr）
	Stream(options StreamOptions) error
}

type streamCreator interface {
	CreateStream(headers http.Header) (httpstream.Stream, error)
}

type streamProtocolHandler interface {
	stream(conn streamCreator) error
}

// streamExecutor handles transporting standard shell streams over an httpstream connection.
// streamExecutor处理通过一个httpstream连接处理传输标准的shell streams
type streamExecutor struct {
	upgrader  spdy.Upgrader
	transport http.RoundTripper

	method    string
	url       *url.URL
	protocols []string
}

// NewSPDYExecutor connects to the provided server and upgrades the connection to
// multiplexed bidirectional streams.
// NewSPDYExecutor和提供的server相连并且将连接升级为多路复用的双向流
func NewSPDYExecutor(config *restclient.Config, method string, url *url.URL) (Executor, error) {
	return NewSPDYExecutorForProtocols(
		config, method, url,
		// 优先级从高到低排列
		remotecommand.StreamProtocolV4Name,
		remotecommand.StreamProtocolV3Name,
		remotecommand.StreamProtocolV2Name,
		remotecommand.StreamProtocolV1Name,
	)
}

// NewSPDYExecutorForProtocols connects to the provided server and upgrades the connection to
// multiplexed bidirectional streams using only the provided protocols. Exposed for testing, most
// callers should use NewSPDYExecutor.
// NewSPDYExecutorForProtocols连接指定的server，并且利用给定的protocols将连接更新为多路复用的双向流
func NewSPDYExecutorForProtocols(config *restclient.Config, method string, url *url.URL, protocols ...string) (Executor, error) {
	// config一般为空
	// wrapper的类型为http.RoundTripper
	wrapper, upgradeRoundTripper, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, err
	}
	// 在已有的wrapper基础之上添加log
	wrapper = transport.DebugWrappers(wrapper)
	return &streamExecutor{
		upgrader:  upgradeRoundTripper,
		transport: wrapper,
		// method，url，protocol都直接赋值
		method:    method,
		url:       url,
		protocols: protocols,
	}, nil
}

// Stream opens a protocol streamer to the server and streams until a client closes
// the connection or the server disconnects.
// Stream 打开一个通往server的protocol streamer，保持stream直到client或者server关闭连接
func (e *streamExecutor) Stream(options StreamOptions) error {
	// 创建一个到stream server的连接
	req, err := http.NewRequest(e.method, e.url.String(), nil)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	conn, protocol, err := spdy.Negotiate(
		e.upgrader,
		&http.Client{Transport: e.transport},
		req,
		e.protocols...,
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	var streamer streamProtocolHandler

	switch protocol {
	case remotecommand.StreamProtocolV4Name:
		streamer = newStreamProtocolV4(options)
	case remotecommand.StreamProtocolV3Name:
		streamer = newStreamProtocolV3(options)
	case remotecommand.StreamProtocolV2Name:
		streamer = newStreamProtocolV2(options)
	case "":
		glog.V(4).Infof("The server did not negotiate a streaming protocol version. Falling back to %s", remotecommand.StreamProtocolV1Name)
		fallthrough
	case remotecommand.StreamProtocolV1Name:
		streamer = newStreamProtocolV1(options)
	}

	return streamer.stream(conn)
}
