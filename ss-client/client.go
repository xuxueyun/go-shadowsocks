package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	comm "github.com/go-shadowsocks/common"
)

var (
	errAddrType      = errors.New("socks addr type not supported")
	errVer           = errors.New("socks version not supported")
	errMethod        = errors.New("socks only support 1 method now")
	errAuthExtraData = errors.New("socks authentication get extra data")
	errReqExtraData  = errors.New("socks request get extra data")
	errCmd           = errors.New("socks command not supported")
)

const (
	socksVer5       = 5
	socksCmdConnect = 1
)

var debug comm.DebugLog

//handshake:
func handshake(conn net.Conn) (err error) {
	debug.Println("start handshake...")
	buf := make([]byte, 258)
	comm.SetReadTimeout(conn)
	var n int
	if n, err = io.ReadAtLeast(conn, buf, 2); err != nil {
		return err
	}
	ver := buf[0]
	if ver != socksVer5 {
		return errVer
	}
	nmethod := int(buf[1])
	msglen := nmethod + 2
	if n == msglen { //general handshake
	} else if n < msglen { //need password & username
		if _, err = io.ReadFull(conn, buf); err != nil {
			return
		}
	} else {
		return errAuthExtraData
	}
	_, err = conn.Write([]byte{5, 0})
	debug.Println("finished handshake...")
	return
}

//getRequest: unpack request
func getRequest(conn net.Conn) (rawaddr []byte, host string, err error) {
	const (
		idVer   = 0
		idCmd   = 1
		idType  = 3
		idIP0   = 4
		idDmLen = 4
		idDm0   = 5

		typeIPv4 = 1
		typeDm   = 3
		typeIPv6 = 4

		lenIPv4   = 3 + 1 + net.IPv4len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv4 + 2port
		lenIPv6   = 3 + 1 + net.IPv6len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv6 + 2port
		lenDmBase = 3 + 1 + 1 + 2           // 3 + 1addrType + 1addrLen + 2port, plus addrLen
	)
	comm.SetReadTimeout(conn)
	buf := make([]byte, 263)
	var n int
	if n, err = io.ReadAtLeast(conn, buf, 5); err != nil { // VER+CMD+RSV+ATYP=4
		return
	}
	if buf[idVer] != socksVer5 {
		err = errVer
		return
	}
	if buf[idCmd] != socksCmdConnect {
		err = errCmd
		return
	}

	reqLen := -1
	switch buf[idType] {
	case typeIPv4:
		reqLen = lenIPv4
	case typeIPv6:
		reqLen = lenIPv6
	case typeDm:
		reqLen = int(buf[idDmLen]) + lenDmBase
	default:
		err = errAddrType
		return
	}

	if n == reqLen {
		//common case, do nothing
	} else if n < reqLen { // rare case
		if _, err = io.ReadFull(conn, buf[n:reqLen]); err != nil {
			return
		}
	} else {
		err = errReqExtraData
		return
	}
	rawaddr = buf[idType:reqLen]
	if debug {
		switch buf[idType] {
		case typeIPv4:
			host = net.IP(buf[idIP0 : idIP0+net.IPv4len]).String()
		case typeDm:
			host = net.IP(buf[idDm0 : idDm0+buf[idDmLen]]).String()
		case typeIPv6:
			host = net.IP(buf[idIP0 : idIP0+net.IPv6len]).String()
		}
		port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
		host = net.JoinHostPort(host, strconv.Itoa(int(port)))
		debug.Println("visit host:", host)
	}
	return
}

//createServerConn: connect to remote
func createServerConn(rawaddr []byte, addr string) (remote *comm.Conn, err error) {
	serverport := server.srvCipher.srv.Server + ":" + strconv.Itoa(server.srvCipher.srv.Port)
	remote, err = comm.DialWithRawAddr(rawaddr, serverport, server.srvCipher.cipher)
	if err != nil {
		log.Println("error connecting to shadowsocks server:", err)
		return nil, err
	}
	debug.Printf("connect to remote:%s success", serverport)
	return
}

func handleConnection(conn net.Conn) {
	debug.Printf("socks connect from %s\n", conn.RemoteAddr().String())
	closed := false
	defer func() {
		if !closed {
			conn.Close()
		}
	}()

	if err := handshake(conn); err != nil {
		debug.Printf("handshake: %s", err)
		return
	}
	rawaddr, addr, err := getRequest(conn)
	if err != nil {
		debug.Printf("error get request: %s\n", err)
		return
	}
	_, err = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
	if err != nil {
		debug.Println("send connection confirmation:", err)
		return
	}
	remote, err := createServerConn(rawaddr, addr)
	if err != nil {
		debug.Println("connect to remote error: ", err)
		return
	}
	defer func() {
		if !closed {
			remote.Close()
		}
	}()
	go comm.PipeThenClose(conn, remote)
	comm.PipeThenClose(remote, conn)
	closed = true
	debug.Println("closed connection to", addr)
}

func run(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	debug.Printf("start listen socks5 port%v\n", addr)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		go handleConnection(conn)
	}
}

//ServerCipher 服务端数据结构
type ServerCipher struct {
	srv    comm.Server
	cipher *comm.Cipher
}

var server struct {
	srvCipher ServerCipher
}

func main() {
	var configPath string
	var version bool
	//var cmdConfig comm.Config

	flag.BoolVar((*bool)(&debug), "d", false, "debug mode")
	flag.BoolVar((*bool)(&version), "v", false, "current version")
	flag.StringVar(&configPath, "c", os.Getenv("HOME")+"/.shadowsocks/config.json", "config path")
	flag.Parse()

	if version {
		comm.PrintVersion()
		os.Exit(0)
	}
	comm.SetDebug(debug)
	debug.Println("loading config file: ", configPath)
	config, err := comm.ParseConfig(configPath)
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}

	if len(config.Servers) > 0 {
		srv := config.Servers[0]
		if srv.Password == "" || srv.Port == 0 {
			log.Println("password or port cannot be empty")
			os.Exit(1)
		}

		if srv.Method == "" {
			srv.Method = "chacha20-ietf-poly1305"
		}

		err := comm.CheckCipherMethod(srv.Method)
		if err != nil {
			log.Println(err)
			os.Exit(1)
		}
		server.srvCipher.srv = srv
		server.srvCipher.cipher = comm.NewCipher(srv)
		run(":" + strconv.Itoa(config.LocalPort))
	} else {
		log.Println("config file has some errors")
		os.Exit(1)
	}
}
