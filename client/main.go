package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	socks5 "github.com/things-go/go-socks5"
)

type DataPackage struct {
	streamId  uint32
	Operation string
	data      []byte
}

type fakeAddr struct {
	NetworkType string
	RemoteAddr  string
}

func (f fakeAddr) Network() string { return f.NetworkType }
func (f fakeAddr) String() string  { return f.RemoteAddr }

type HTTPTunnel struct {
	connects map[uint32]*HTTPTunnelConnect
	connMu   sync.RWMutex

	ReadChan  chan DataPackage
	WriteChan chan DataPackage

	Done      chan struct{}
	closeOnce sync.Once

	LocalAddr net.TCPAddr
}

func isDebugEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("DEBUG")))
	return v == "1" || v == "true" || v == "yes" || v == "on" || v == "debug"
}

var debugEnabled = isDebugEnabled()

func dlogf(format string, args ...any) {
	if debugEnabled {
		log.Printf(format, args...)
	}
}

func (t *HTTPTunnel) StartUpLoad(url string) (net.Conn, error) {
	log.Printf("[upload] starting upload stream: %s", url)
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		dlogf("[upload] writer goroutine started")
		for {
			select {
			case d := <-t.WriteChan:
				line := encodeFrame(d) + "\n"
				if _, err := pw.Write([]byte(line)); err != nil {
					dlogf("[upload] pipe write failed: %v", err)
					return
				}
			case <-t.Done:
				dlogf("[upload] writer goroutine stopping by tunnel done")
				return
			}
		}
	}()

	req, err := http.NewRequest("POST", url, pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		dlogf("[upload] request failed: %v", err)
		return nil, err
	}
	dlogf("[upload] connected, response status=%s", resp.Status)
	go func() {
		defer resp.Body.Close()
		_, copyErr := io.Copy(io.Discard, resp.Body)
		if copyErr != nil {
			dlogf("[upload] response drain error: %v", copyErr)
			return
		}
		dlogf("[upload] response body closed by peer")
	}()
	return nil, nil
}

// 下载隧道，从服务器接收数据包
func (t *HTTPTunnel) StartDownload(url string) {
	log.Printf("[download] starting download stream: %s", url)
	resp, err := http.Get(url)
	if err != nil {
		dlogf("[download] request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	dlogf("[download] connected, response status=%s", resp.Status)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for {
		select {
		case <-t.Done:
			dlogf("[download] stopping by tunnel done")
			resp.Body.Close()
			return
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					dlogf("[download] scanner failed: %v", err)
					panic(err)
				}
				dlogf("[download] stream ended by peer (scanner EOF)")
				return
			}

			line := strings.TrimSpace(scanner.Text())
			dlogf("收到原始数据: %q", line)
			if line == "" {
				continue
			}

			streamId, op, body, err := decodeFrame(line)
			if err != nil {
				dlogf("解析帧失败: %v, 原文: %q", err, line)
				continue
			}

			if op == "closed" {
				dlogf("收到关闭信号 streamId=%d", streamId)
				c, ok := t.GetConnect(streamId)
				if !ok {
					dlogf("未找到 streamId: %d,也许已关闭", streamId)
					continue
				}
				c.Close()
				dlogf("连接已关闭: streamId=%d", streamId)
				continue
			} else {
				func(streamId uint32, data []byte) {
					dlogf("准备处理数据包: streamId=%d, data=%d bytes\n", streamId, len(data))
					c, ok := t.GetConnect(streamId)
					if !ok {
						dlogf("未找到 streamId: %d", streamId)
						return
					}
					c.ReadChan <- data
					dlogf("收到数据包: streamId=%d, data=%d bytes", streamId, len(data))
				}(streamId, body)
			}
		}
	}
}

func (t *HTTPTunnel) GetConnect(streamId uint32) (*HTTPTunnelConnect, bool) {
	t.connMu.RLock()
	defer t.connMu.RUnlock()
	c, ok := t.connects[streamId]
	return c, ok
}

func (t *HTTPTunnel) AddConnect(streamId uint32, c *HTTPTunnelConnect) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	t.connects[streamId] = c
}

func (t *HTTPTunnel) HTTPConnectDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var streamId uint32
	for {
		streamId = uint32(time.Now().UnixNano())
		if _, exists := t.GetConnect(streamId); !exists {
			break
		}
	}
	dlogf("[dial] request start: streamId=%d network=%s addr=%s", streamId, network, addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q: %w", port, err)
	}
	var connect *HTTPTunnelConnect
	switch network {
	case "tcp", "tcp4", "tcp6":
		// 发送创建tcp连接的信号，服务器会根据这个信号创建对应的连接
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.Done:
			return nil, io.EOF
		case t.WriteChan <- DataPackage{
			streamId:  streamId,
			Operation: "connect",
			data:      []byte(fmt.Sprintf("%s:%s", network, addr)),
		}:
			connect = &HTTPTunnelConnect{
				StreamID: streamId,
				Tunnel:   t,
				ReadChan: make(chan []byte, 100),
				close:    make(chan struct{}),
				Addr: &net.TCPAddr{
					IP:   net.ParseIP(host),
					Port: portNum,
				},
				ReadDeadline:  time.Time{},
				WriteDeadline: time.Time{},
			}
		}

	case "udp", "udp4", "udp6":
		// 发送创建udp连接的信号，服务器会根据这个信号创建对应的连接
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.Done:
			return nil, io.EOF
		case t.WriteChan <- DataPackage{
			streamId:  streamId,
			Operation: "connect",
			data:      []byte(fmt.Sprintf("%s:%s", network, addr)),
		}:
			connect = &HTTPTunnelConnect{
				StreamID: streamId,
				Tunnel:   t,
				ReadChan: make(chan []byte, 100),
				close:    make(chan struct{}),
				// Ignore port error because we have already validated the port above
				Addr: &net.UDPAddr{
					IP:   net.ParseIP(host),
					Port: portNum,
				},
				ReadDeadline:  time.Time{},
				WriteDeadline: time.Time{},
			}
		}

	default:
		return nil, fmt.Errorf("unsupported network: %s", network)
	}

	t.AddConnect(streamId, connect)
	dlogf("[dial] conn created: streamId=%d, network=%s, addr=%s", streamId, network, addr)

	return connect, nil
}

func (t *HTTPTunnel) Close() {
	// TODO : 关闭对端所有连接
	t.closeOnce.Do(func() {
		dlogf("[tunnel] close invoked")
		close(t.Done)
	})
}

func encodeFrame(p DataPackage) string {
	b64 := base64.StdEncoding.EncodeToString(p.data)
	return fmt.Sprintf("%d:%s:%s", p.streamId, p.Operation, b64)
}

func decodeFrame(line string) (uint32, string, []byte, error) {
	parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
	if len(parts) != 3 {
		return 0, "", nil, fmt.Errorf("invalid frame")
	}

	var streamId uint32
	if _, err := fmt.Sscanf(parts[0], "%d", &streamId); err != nil {
		return 0, "", nil, fmt.Errorf("invalid stream id: %w", err)
	}

	var op string
	op = parts[1]
	if op != "connect" && op != "close" && op != "data" && op != "closed" {
		return 0, "", nil, fmt.Errorf("invalid operation: %s", op)
	}

	body, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, "", nil, fmt.Errorf("invalid base64 payload: %w", err)
	}

	return streamId, op, body, nil
}

type HTTPTunnelConnect struct {
	StreamID uint32
	Tunnel   *HTTPTunnel

	ReadChan chan []byte
	close    chan struct{}

	Addr          net.Addr
	ReadDeadline  time.Time
	WriteDeadline time.Time
	readBuf       []byte

	Mu        sync.Mutex
	closeOnce sync.Once
}

func waitDeadline(deadline time.Time) <-chan time.Time {
	if deadline.IsZero() {
		return nil
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		ch := make(chan time.Time)
		close(ch)
		return ch
	}
	return time.After(timeout)
}

func (c *HTTPTunnelConnect) Write(data []byte) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}

	c.Mu.Lock()
	deadline := c.WriteDeadline
	c.Mu.Unlock()
	dlogf("准备发送数据包: streamId=%d, data=%d bytes\n", c.StreamID, len(data))

	payload := append([]byte(nil), data...)

	select {
	case <-waitDeadline(deadline):
		dlogf("[write] timeout: streamId=%d", c.StreamID)
		return 0, fmt.Errorf("write timeout")
	case <-c.close:
		dlogf("[write] local close detected: streamId=%d", c.StreamID)
		return 0, io.ErrClosedPipe
	case <-c.Tunnel.Done:
		dlogf("[write] tunnel done detected: streamId=%d", c.StreamID)
		return 0, io.EOF
	case c.Tunnel.WriteChan <- DataPackage{
		streamId:  c.StreamID,
		Operation: "data",
		data:      payload,
	}:
	}
	return len(data), nil
}

func (c *HTTPTunnelConnect) Close() error {
	c.closeOnce.Do(func() {
		dlogf("[conn] close invoked: streamId=%d\n", c.StreamID)
		c.Tunnel.connMu.Lock()
		// 关闭服务器端的连接
		c.Tunnel.WriteChan <- DataPackage{
			streamId:  c.StreamID,
			Operation: "close",
		}
		// 关闭本地的connect，并从隧道的连接列表中删除
		delete(c.Tunnel.connects, c.StreamID)
		close(c.close)
		c.Tunnel.connMu.Unlock()
	})
	return nil
}

func (c *HTTPTunnelConnect) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	c.Mu.Lock()
	deadline := c.ReadDeadline
	if len(c.readBuf) > 0 {
		n = copy(p, c.readBuf)
		if n < len(c.readBuf) {
			c.readBuf = append([]byte(nil), c.readBuf[n:]...)
		} else {
			c.readBuf = nil
		}
		c.Mu.Unlock()
		return n, nil
	}
	c.Mu.Unlock()

	dlogf("等待读取数据包: streamId=%d\n", c.StreamID)
	for {
		select {
		case <-waitDeadline(deadline):
			dlogf("[read] timeout: streamId=%d", c.StreamID)
			return 0, fmt.Errorf("read timeout")
		case data := <-c.ReadChan:
			if len(data) == 0 {
				continue
			}
			dlogf("准备读取数据包: streamId=%d, data=%d bytes\n", c.StreamID, len(data))
			n = copy(p, data)
			if n < len(data) {
				c.Mu.Lock()
				c.readBuf = append(c.readBuf, data[n:]...)
				c.Mu.Unlock()
			}
			return n, nil
		case <-c.close:
			dlogf("[read] local close detected: streamId=%d", c.StreamID)
			return 0, io.EOF
		case <-c.Tunnel.Done:
			dlogf("[read] tunnel done detected: streamId=%d", c.StreamID)
			return 0, io.EOF
		}
	}
}

func (c *HTTPTunnelConnect) SetDeadline(t time.Time) error {
	c.Mu.Lock()
	c.ReadDeadline = t
	c.WriteDeadline = t
	c.Mu.Unlock()
	return nil
}

func (c *HTTPTunnelConnect) SetReadDeadline(t time.Time) error {
	c.Mu.Lock()
	c.ReadDeadline = t
	c.Mu.Unlock()
	return nil
}

func (c *HTTPTunnelConnect) SetWriteDeadline(t time.Time) error {
	c.Mu.Lock()
	c.WriteDeadline = t
	c.Mu.Unlock()
	return nil
}

func (c *HTTPTunnelConnect) LocalAddr() net.Addr {
	return &c.Tunnel.LocalAddr
}

func (c *HTTPTunnelConnect) RemoteAddr() net.Addr {
	return c.Addr
}

func (t *HTTPTunnel) Start(baseURL string) {
	dlogf("[tunnel] starting with baseURL=%s", baseURL)
	go t.StartDownload(baseURL + "/send")
	go t.StartUpLoad(baseURL + "/receive")
	dlogf("[tunnel] upload/download goroutines started")
	<-t.Done
	dlogf("[tunnel] done signal received")
	t.Close()

}

func main() {

	Port := flag.Int("port", 1080, "本地SOCKS5代理监听端口，默认1080")
	Host := flag.String("host", "0.0.0.0", "本地SOCKS5代理监听地址，默认全开放")
	Target := flag.String("target", "http://127.0.0.1:5000", "隧道服务器地址")

	flag.Parse()

	baseURL := *Target

	tunnel := &HTTPTunnel{
		connects:  make(map[uint32]*HTTPTunnelConnect),
		ReadChan:  make(chan DataPackage, 100),
		WriteChan: make(chan DataPackage, 100),
		Done:      make(chan struct{}),
		LocalAddr: net.TCPAddr{
			IP:   net.ParseIP(*Host),
			Port: *Port,
			Zone: "",
		},
	}

	server := socks5.NewServer(
		socks5.WithDial(tunnel.HTTPConnectDial),
	)

	go tunnel.Start(baseURL)

	log.Printf("SOCKS5 listening on %s, tunnel base url: %s", tunnel.LocalAddr.String(), baseURL)
	if err := server.ListenAndServe("tcp", tunnel.LocalAddr.String()); err != nil {
		log.Fatalf("SOCKS5 server failed: %v", err)
	}

}
