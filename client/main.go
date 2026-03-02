package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
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

	LocalAddr fakeAddr
}

func (t *HTTPTunnel) StartUpLoad(url string) (net.Conn, error) {
	log.Printf("[upload] starting upload stream: %s", url)
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		log.Printf("[upload] writer goroutine started")
		for {
			select {
			case d := <-t.WriteChan:
				line := encodeFrame(d) + "\n"
				if _, err := pw.Write([]byte(line)); err != nil {
					log.Printf("[upload] pipe write failed: %v", err)
					return
				}
			case <-t.Done:
				log.Printf("[upload] writer goroutine stopping by tunnel done")
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
		log.Printf("[upload] request failed: %v", err)
		return nil, err
	}
	log.Printf("[upload] connected, response status=%s", resp.Status)
	go func() {
		defer resp.Body.Close()
		_, copyErr := io.Copy(io.Discard, resp.Body)
		if copyErr != nil {
			log.Printf("[upload] response drain error: %v", copyErr)
			return
		}
		log.Printf("[upload] response body closed by peer")
	}()
	return nil, nil
}

// 下载隧道，从服务器接收数据包
func (t *HTTPTunnel) StartDownload(url string) {
	log.Printf("[download] starting download stream: %s", url)
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("[download] request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("[download] connected, response status=%s", resp.Status)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for {
		select {
		case <-t.Done:
			log.Printf("[download] stopping by tunnel done")
			resp.Body.Close()
			return
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					log.Printf("[download] scanner failed: %v", err)
					panic(err)
				}
				log.Printf("[download] stream ended by peer (scanner EOF)")
				return
			}

			line := strings.TrimSpace(scanner.Text())
			log.Printf("收到原始数据: %q", line)
			if line == "" {
				continue
			}

			streamId, body, err := decodeFrame(line)
			if err != nil {
				log.Printf("解析帧失败: %v, 原文: %q", err, line)
				continue
			}

			func(streamId uint32, data []byte) {
				log.Printf("准备处理数据包: streamId=%d, data=%d bytes\n", streamId, len(data))
				c, ok := t.GetConnect(streamId)
				if !ok {
					log.Printf("未找到 streamId: %d", streamId)
					return
				}
				c.ReadChan <- data
				log.Printf("收到数据包: streamId=%d, data=%d bytes", streamId, len(data))
			}(streamId, body)
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
	streamId := uint32(time.Now().UnixNano())
	log.Printf("[dial] request start: streamId=%d network=%s addr=%s", streamId, network, addr)
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
		}

	default:
		return nil, fmt.Errorf("unsupported network: %s", network)
	}

	connect := &HTTPTunnelConnect{
		StreamID: streamId,
		Tunnel:   t,
		ReadChan: make(chan []byte, 100),
		close:    make(chan struct{}),
		Addr: fakeAddr{
			NetworkType: network,
			RemoteAddr:  addr,
		},
		ReadDeadline:  time.Time{},
		WriteDeadline: time.Time{},
	}

	t.AddConnect(streamId, connect)
	log.Printf("[dial] conn created: streamId=%d, network=%s, addr=%s", streamId, network, addr)

	return connect, nil
}

func (t *HTTPTunnel) Close() {
	// TODO : 关闭对端所有连接
	t.closeOnce.Do(func() {
		log.Printf("[tunnel] close invoked")
		close(t.Done)
	})
}

func encodeFrame(p DataPackage) string {
	b64 := base64.StdEncoding.EncodeToString(p.data)
	return fmt.Sprintf("%d:%s:%s", p.streamId, p.Operation, b64)
}

func decodeFrame(line string) (uint32, []byte, error) {
	parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
	if len(parts) != 3 {
		return 0, nil, fmt.Errorf("invalid frame")
	}

	var streamId uint32
	if _, err := fmt.Sscanf(parts[0], "%d", &streamId); err != nil {
		return 0, nil, fmt.Errorf("invalid stream id: %w", err)
	}

	var op string
	op = parts[1]
	if op != "connect" && op != "close" && op != "data" {
		return 0, nil, fmt.Errorf("invalid operation: %s", op)
	}

	body, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, fmt.Errorf("invalid base64 payload: %w", err)
	}

	return streamId, body, nil
}

type HTTPTunnelConnect struct {
	StreamID uint32
	Tunnel   *HTTPTunnel

	ReadChan chan []byte
	close    chan struct{}

	Addr fakeAddr

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
	log.Printf("准备发送数据包: streamId=%d, data=%d bytes\n", c.StreamID, len(data))

	payload := append([]byte(nil), data...)

	select {
	case <-waitDeadline(deadline):
		log.Printf("[write] timeout: streamId=%d", c.StreamID)
		return 0, fmt.Errorf("write timeout")
	case <-c.close:
		log.Printf("[write] local close detected: streamId=%d", c.StreamID)
		return 0, io.ErrClosedPipe
	case <-c.Tunnel.Done:
		log.Printf("[write] tunnel done detected: streamId=%d", c.StreamID)
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
		log.Printf("[conn] close invoked: streamId=%d\n", c.StreamID)
		c.Tunnel.connMu.Lock()
		// TODO : 关闭服务器端的连接
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

	log.Printf("等待读取数据包: streamId=%d\n", c.StreamID)
	for {
		select {
		case <-waitDeadline(deadline):
			log.Printf("[read] timeout: streamId=%d", c.StreamID)
			return 0, fmt.Errorf("read timeout")
		case data := <-c.ReadChan:
			if len(data) == 0 {
				continue
			}
			log.Printf("准备读取数据包: streamId=%d, data=%d bytes\n", c.StreamID, len(data))
			n = copy(p, data)
			if n < len(data) {
				c.Mu.Lock()
				c.readBuf = append(c.readBuf, data[n:]...)
				c.Mu.Unlock()
			}
			return n, nil
		case <-c.close:
			log.Printf("[read] local close detected: streamId=%d", c.StreamID)
			return 0, io.EOF
		case <-c.Tunnel.Done:
			log.Printf("[read] tunnel done detected: streamId=%d", c.StreamID)
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
	return &net.TCPAddr{IP: net.IPv4zero, Port: 0}
}

func (c *HTTPTunnelConnect) RemoteAddr() net.Addr {
	return c.Addr
}

func (t *HTTPTunnel) Start(baseURL string) {
	log.Printf("[tunnel] starting with baseURL=%s", baseURL)
	go t.StartDownload(baseURL + "/send")
	go t.StartUpLoad(baseURL + "/receive")
	log.Printf("[tunnel] upload/download goroutines started")
	<-t.Done
	log.Printf("[tunnel] done signal received")
	t.Close()

}

func main() {
	tunnel := &HTTPTunnel{
		connects:  make(map[uint32]*HTTPTunnelConnect),
		ReadChan:  make(chan DataPackage, 100),
		WriteChan: make(chan DataPackage, 100),
		Done:      make(chan struct{}),
		LocalAddr: fakeAddr{
			NetworkType: "socks5",
			RemoteAddr:  "0.0.0.0:1080",
		},
	}

	baseURL := os.Getenv("TUNNEL_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:5000"
	}

	listenAddr := os.Getenv("SOCKS5_LISTEN")
	if listenAddr == "" {
		listenAddr = "127.0.0.1:1080"
	}

	server := socks5.NewServer(
		socks5.WithDial(tunnel.HTTPConnectDial),
	)

	go tunnel.Start(baseURL)

	log.Printf("SOCKS5 listening on %s, tunnel base url: %s", listenAddr, baseURL)
	if err := server.ListenAndServe("tcp", listenAddr); err != nil {
		log.Fatalf("SOCKS5 server failed: %v", err)
	}

}
