package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	socksVersion5 = 0x05

	authNoAcceptable = 0xFF
	authNoAuth       = 0x00

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess              = 0x00
	repGeneralFailure       = 0x01
	repConnectionNotAllowed = 0x02
	repNetworkUnreachable   = 0x03
	repHostUnreachable      = 0x04
	repConnectionRefused    = 0x05
	repTTLExpired           = 0x06
	repCommandNotSupported  = 0x07
	repAddrTypeNotSupported = 0x08
)

type data struct {
	streamId int64
	data     []byte
}

type connect struct {
	streamId int64
	conn     net.Conn
}

type Server struct {
	// 连接管理
	connects   map[int64]connect
	connMu     sync.RWMutex
	listenConn net.Listener //本地监听端口

	// 对端内存马管理
	sendChan chan data
	recvChan chan data

	// 其他必要的字段
	Done chan struct{}
}

func NewServer(listenAddr string) (*Server, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听失败 %s: %w", listenAddr, err)
	}

	return &Server{
		connects:   make(map[int64]connect),
		listenConn: listener,
		sendChan:   make(chan data, 100),
		recvChan:   make(chan data, 100),
		Done:       make(chan struct{}),
	}, nil
}

func encodeFrame(streamId int64, payload []byte) string {
	b64 := base64.StdEncoding.EncodeToString(payload)
	return fmt.Sprintf("%d:%s", streamId, b64)
}

func decodeFrame(line string) (int64, []byte, error) {
	parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
	if len(parts) != 2 {
		return 0, nil, fmt.Errorf("invalid frame")
	}

	var streamId int64
	if _, err := fmt.Sscanf(parts[0], "%d", &streamId); err != nil {
		return 0, nil, fmt.Errorf("invalid stream id: %w", err)
	}

	body, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, nil, fmt.Errorf("invalid base64 payload: %w", err)
	}

	return streamId, body, nil
}

func (s *Server) setConn(c connect) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.connects[c.streamId] = c
}

func (s *Server) getConn(streamId int64) (connect, bool) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	c, ok := s.connects[streamId]
	return c, ok
}

func (s *Server) removeConn(streamId int64) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	delete(s.connects, streamId)
}

// 上传隧道，发送数据包到服务器
func (s *Server) startUpLoad(url string) (net.Conn, error) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			select {
			case d := <-s.sendChan:
				line := encodeFrame(d.streamId, d.data) + "\n"
				if _, err := pw.Write([]byte(line)); err != nil {
					log.Printf("upload 写入失败: %v", err)
					return
				}
			case <-s.Done:
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
		fmt.Println("请求失败:", err)
		return nil, err
	}
	go func() {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	return nil, nil
}

// 下载隧道，从服务器接收数据包
func (s *Server) startDownload(url string) {
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("请求失败:", err)
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for {
		select {
		case <-s.Done:
			resp.Body.Close()
			return
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					log.Printf("下载流读取出错: %v", err)
					panic(err)
				}
				return
			}

			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			streamId, body, err := decodeFrame(line)
			if err != nil {
				log.Printf("解析帧失败: %v, 原文: %q", err, line)
				continue
			}

			s.recvChan <- data{streamId: streamId, data: body}
		}
	}
}

func (s *Server) distributeData() {
	for {
		d := <-s.recvChan
		c, ok := s.getConn(d.streamId)
		if !ok {
			continue
		}
		if _, err := c.conn.Write(d.data); err != nil {
			log.Printf("回写本地连接失败 stream=%d: %v", d.streamId, err)
			_ = c.conn.Close()
			s.removeConn(d.streamId)
		}
	}
}

// func (s *Server) testWrite() {
// 	for {
// 		time.Sleep(time.Second)
// 		s.sendChan <- data{streamId: 0, data: []byte("Hello from client!")}
// 	}
// }

func (s *Server) Start(dialTimeout time.Duration) {
	go s.startUpLoad("http://127.0.0.1:5000/receive")

	go s.startDownload("http://127.0.0.1:5000/send")
	go s.distributeData()

	log.Printf("SOCKS5 代理服务器已启动，监听 %s", s.listenConn.Addr())

	for {
		clientConn, err := s.listenConn.Accept()
		if err != nil {
			log.Printf("接受连接失败: %v", err)
			continue
		}
		streamId := rand.Int63()
		s.setConn(connect{streamId: streamId, conn: clientConn})

		go s.handleClient(streamId, dialTimeout)
	}
}

func main() {
	listenAddr := flag.String("listen", ":1080", "SOCKS5 监听地址")
	dialTimeout := flag.Duration("dial-timeout", 5*time.Second, "连接目标超时时间")
	flag.Parse()

	server, err := NewServer(*listenAddr)
	if err != nil {
		log.Fatalf("创建服务器失败: %v", err)
	}
	server.Start(*dialTimeout)
}

func (s *Server) handleClient(id int64, dialTimeout time.Duration) {
	c, ok := s.getConn(id)
	if !ok {
		return
	}
	defer c.conn.Close()
	defer s.removeConn(id)

	buf := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			s.sendChan <- data{streamId: id, data: payload}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("读取本地连接失败 stream=%d: %v", id, err)
			}
			return
		}
	}

}
