/*
Copyright 2017 The Kubernetes Authors.

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

package server

import (
	"fmt"
	"io"
	"math"
	"net"

	"golang.org/x/net/context"
	k8snet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/utils/exec"
)

func newStreamServer(c *criContainerdService, addr, port string) (streaming.Server, error) {
	if addr == "" {
		a, err := k8snet.ChooseBindAddress(nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get stream server address: %v", err)
		}
		addr = a.String()
	}
	// config使用streaming的DefaultConfig
	config := streaming.DefaultConfig
	config.Addr = net.JoinHostPort(addr, port)
	// runtime实现了streaming server指定的Exec,Attach和PortForward三个方法
	runtime := newStreamRuntime(c)
	return streaming.NewServer(config, runtime)
}

type streamRuntime struct {
	c *criContainerdService
}

func newStreamRuntime(c *criContainerdService) streaming.Runtime {
	return &streamRuntime{c: c}
}

// Exec executes a command inside the container. exec.ExitError is returned if the command
// returns non-zero exit code.
// Exec在容器里执行一条命令，如果执行的命令返回的是非零的exit code，则返回exec.ExitError
func (s *streamRuntime) Exec(containerID string, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser,
	tty bool, resize <-chan remotecommand.TerminalSize) error {
	exitCode, err := s.c.execInContainer(context.Background(), containerID, execOptions{
		cmd:    cmd,
		stdin:  stdin,	// true
		stdout: stdout,	// true
		stderr: stderr,	// false
		tty:    tty,	// true
		resize: resize,
	})
	if err != nil {
		return fmt.Errorf("failed to exec in container: %v", err)
	}
	if *exitCode == 0 {
		return nil
	}
	return &exec.CodeExitError{
		Err:  fmt.Errorf("error executing command %v, exit code %d", cmd, *exitCode),
		Code: int(*exitCode),
	}
}

func (s *streamRuntime) Attach(containerID string, in io.Reader, out, err io.WriteCloser, tty bool,
	resize <-chan remotecommand.TerminalSize) error {
	return s.c.attachContainer(context.Background(), containerID, in, out, err, tty, resize)
}

func (s *streamRuntime) PortForward(podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	if port <= 0 || port > math.MaxUint16 {
		return fmt.Errorf("invalid port %d", port)
	}
	return s.c.portForward(podSandboxID, port, stream)
}

// handleResizing spawns a goroutine that processes the resize channel, calling resizeFunc for each
// remotecommand.TerminalSize received from the channel. The resize channel must be closed elsewhere to stop the
// goroutine.
func handleResizing(resize <-chan remotecommand.TerminalSize, resizeFunc func(size remotecommand.TerminalSize)) {
	if resize == nil {
		return
	}

	go func() {
		defer runtime.HandleCrash()

		for {
			size, ok := <-resize
			if !ok {
				return
			}
			if size.Height < 1 || size.Width < 1 {
				continue
			}
			resizeFunc(size)
		}
	}()
}
