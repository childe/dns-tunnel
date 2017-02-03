package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/golang/glog"
)

var options = &struct {
	listen       string
	dnsServer    string
	domain       string
	readTimeout  uint64
	dnsBatchSize int
}{}

var dnsServerAddr *net.UDPAddr
var hostPattern *regexp.Regexp

func init() {
	flag.StringVar(&options.listen, "listen", "127.0.0.1:8080", "default 127.0.0.1:8080")
	flag.StringVar(&options.dnsServer, "dns", "", "dns server, like 192.168.0.1:53")
	flag.StringVar(&options.domain, "domain", "", "domain required, like yourdomain.me")
	flag.Uint64Var(&options.readTimeout, "timeout", 10000, "timeout(ms) read from proxy server")
	flag.IntVar(&options.dnsBatchSize, "dns-batch-size", 500, "dns request maybe unsuccessful if larger than this")
	flag.Parse()

	if options.domain == "" {
		flag.Usage()
		glog.Fatalln("domain requrired")
	}

	if options.dnsServer == "" {
		flag.Usage()
		glog.Fatalln("dns server requrired")
	}
}

func init() {
	var err error
	hostPattern, err = regexp.Compile(`(?:[A-Za-z]+(?:\+[A-Za-z+]+)?://)?(?:[a-zA-Z0-9._-]+(?::[^@]*)?@)?\b([^/?#]+)\b`)
	if err != nil {
		panic(err)
	}

	dnsServerAddr, err = net.ResolveUDPAddr("udp", options.dnsServer)
	if err != nil {
		panic(err)
	}
}

func getResponse(dnsConn *net.UDPConn, conn net.Conn, closed *bool) {
	timeout := time.Millisecond * time.Duration(options.readTimeout)
	for {

		buffer := make([]byte, 65535)
		for {
			dnsConn.SetReadDeadline(time.Now().Add(timeout))
			n, err := dnsConn.Read(buffer)
			if err != nil {
				glog.Errorf("read from proxy server error: %s", err)
				*closed = true
				return
			}
			if glog.V(9) {
				glog.Infof("read %d bytes from proxy server", n)
			}

			offset := int(binary.BigEndian.Uint32(buffer[:4]))
			if glog.V(9) {
				glog.Infof("offset %d", offset)
			}
		}
	}
}

// the startPositon|FragmentSize Protocol
// host is nil if start is not 0
func fakeDNSRequestEncode(buffer, host []byte, start int) []byte {
	rst := make([]byte, 8+len(host)+len(buffer))
	if glog.V(9) {
		glog.Infof("fakeDNSRequestEncode buffer size: %d", len(buffer))
		glog.Infof("fakeDNSRequestEncode start postion: %d", start)
	}

	now := time.Now().UnixNano()
	domain := fmt.Sprintf("%d.%s", now, options.domain)

	binary.BigEndian.PutUint16(rst, uint16(now))
	var b uint16
	b = 0
	//b |= (0 << 15) //QR
	//b |= (0 << 11) //OPcode
	//b |= (0 << 10) //AA
	//b |= (0 << 9) //TC
	b |= (1 << 8) //RD
	//b |= (0 << 7) //RA
	//b |= 0 //rcode
	binary.BigEndian.PutUint16(rst[2:], b)

	binary.BigEndian.PutUint16(rst[4:], 1)
	binary.BigEndian.PutUint16(rst[6:], 0)
	binary.BigEndian.PutUint16(rst[8:], 0)
	binary.BigEndian.PutUint16(rst[10:], 0)

	offset := 12

	for _, part := range strings.Split(domain, ".") {
		rst[offset] = uint8(len(part))
		offset += 1
		copy(rst[offset:], []byte(part))
		offset += len(part)
	}
	rst[offset] = 0
	offset += 1

	binary.BigEndian.PutUint16(rst[offset:], 1)
	offset += 2
	binary.BigEndian.PutUint16(rst[offset:], 1)
	offset += 2

	// fragment start position
	binary.BigEndian.PutUint32(rst[offset:], uint32(start))
	offset += 4

	// put host length and host
	if start == 0 {
		binary.BigEndian.PutUint32(rst[offset:], uint32(len(host)))
		offset += 4
		copy(rst[offset:], host)
		offset += len(host)
	}

	copy(rst[offset:], buffer)

	return rst
}

/*
set error if could not get correct host
return nil,nil if first line is not covered yet
*/
func getHostFromFirstRequestBuffer(buffer []byte) ([]byte, error) {
	if glog.V(10) {
		glog.Infof("try to get host from %s", string(buffer))
	}
	urlStart := 0
	for i := 0; i < len(buffer); i++ {
		if buffer[i] == ' ' {
			if urlStart == 0 {
				urlStart = i + 1
			} else {
				rst := hostPattern.FindSubmatch(buffer[urlStart:i])
				if len(rst) != 2 {
					return nil, fmt.Errorf("could not get host from %s", string(buffer[:i]))
				}
				return rst[1], nil
			}
		}
	}
	return nil, nil
}

/*
get stream from real client, and parse HOST from the first line.
then send the stream to proxy server slice by slice
*/
func p(conn *net.TCPConn) {
	defer glog.Infof("%s is closed", conn.RemoteAddr())
	defer conn.Close()

	var err error

	dnsConn, err := net.DialUDP("udp", nil, dnsServerAddr)
	if err != nil {
		glog.Errorf("could not dial to %s", dnsServerAddr)
		return
	}

	var n, offset int
	var host []byte
	var previousBuffer, buffer []byte
	var closed bool
	offset = 0
	closed = false
	go getResponse(dnsConn, conn, &closed)

	buffer = make([]byte, options.dnsBatchSize)
	previousBuffer = make([]byte, 0)
	for closed == false {
		n, err = conn.Read(buffer)
		if err == io.EOF {
			glog.Errorf("%s real client send EOF before response", conn.RemoteAddr())
			return
		}

		// get host from first line
		if previousBuffer != nil {
			buffer = append(previousBuffer, buffer[:n]...)
		}
		host, err = getHostFromFirstRequestBuffer(buffer)

		if err != nil {
			glog.Errorln(err.Error())
			return
		}

		// url is longer than options.dnsBatchSize
		if host == nil {
			previousBuffer = append(previousBuffer, buffer[:n]...)
			continue
		}
		glog.V(2).Infof("get host[%s] from request", host)

		dnsReqeust := fakeDNSRequestEncode(buffer, host, offset)
		offset += len(buffer)
		dnsConn.Write(dnsReqeust)
	}
}

func main() {
	tcpAddr, err := net.ResolveTCPAddr("tcp", options.listen)
	if err != nil {
		glog.Fatalf("could not resolve tcp address[%s]: %s", options.listen, err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		glog.Fatalf("could not listen on %s: %s", options.listen, err)
	}
	glog.Infof("listen on %s", options.listen)

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			glog.Errorf("Accept error: %s", err)
		} else {
			go p(conn)
		}
	}
}
