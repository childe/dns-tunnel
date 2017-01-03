package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

var BATCH_SIZE = 500 // dns request maybe unsuccessful if larger than this

var OUTOFMAXSIZE = errors.New("OUTOFMAXSIZE")

var options = &struct {
	listen    string
	dnsServer string
	domain    string
	timeout   uint64
}{}

var dnsServerAddr *net.UDPAddr

func init() {
	flag.StringVar(&options.listen, "listen", "127.0.0.1:8080", "default 127.0.0.1:8080")
	flag.StringVar(&options.dnsServer, "dns", "", "dns server, like 192.168.0.1:53")
	flag.StringVar(&options.domain, "domain", "", "domain required, like yourdomain.me")
	flag.Uint64Var(&options.timeout, "timeout", 10000, "timeout(ms) read from dns server")
	flag.Parse()

	if options.domain == "" {
		fmt.Println("domain requrired")
		flag.Usage()
		os.Exit(2)
	}

	if options.domain == "" {
		fmt.Println("dns server requrired")
		flag.Usage()
		flag.Usage()
		os.Exit(2)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	var err error
	dnsServerAddr, err = net.ResolveUDPAddr("udp", options.dnsServer)
	if err != nil {
		panic(err)
	}
	log.Println("resolved dns server address")
}

func getResponseFromDNSServer(conn *net.UDPConn) ([]byte, error) {
	response := make([]byte, 65535)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Millisecond * time.Duration(options.timeout)))
		n, err := conn.Read(response)
		if err != nil {
			return nil, err
		}
		log.Printf("read %d bytes from dns server\n", n)
	}
}

// Protocol: totalSize|startPositon|FragmentSize
func fakeDNSRequestEncode(buffer, content []byte, start int) (int, int) {
	log.Printf("buffer size: %d\n", len(buffer))
	log.Printf("start postion: %d\n", start)
	now := time.Now().UnixNano()
	domain := fmt.Sprintf("%d.%s", now, options.domain)
	log.Printf("domain: %s\n", domain)

	binary.BigEndian.PutUint16(buffer, uint16(now))
	var b uint16
	b = 0
	//b |= (0 << 15) //QR
	//b |= (0 << 11) //OPcode
	//b |= (0 << 10) //AA
	//b |= (0 << 9) //TC
	b |= (1 << 8) //RD
	//b |= (0 << 7) //RA
	//b |= 0 //rcode
	binary.BigEndian.PutUint16(buffer[2:], b)

	binary.BigEndian.PutUint16(buffer[4:], 1)
	binary.BigEndian.PutUint16(buffer[6:], 0)
	binary.BigEndian.PutUint16(buffer[8:], 0)
	binary.BigEndian.PutUint16(buffer[10:], 0)

	offset := 12

	for _, part := range strings.Split(domain, ".") {
		buffer[offset] = uint8(len(part))
		offset += 1
		copy(buffer[offset:], []byte(part))
		offset += len(part)
	}
	buffer[offset] = 0
	offset += 1

	binary.BigEndian.PutUint16(buffer[offset:], 1)
	offset += 2
	binary.BigEndian.PutUint16(buffer[offset:], 1)
	offset += 2

	contentSize := len(content)
	// total size
	binary.BigEndian.PutUint32(buffer[offset:], uint32(contentSize))
	offset += 4

	// fragment start position
	binary.BigEndian.PutUint32(buffer[offset:], uint32(start))
	offset += 4

	// fragment length
	fl := 0
	if BATCH_SIZE-offset-4 >= contentSize-start {
		fl = contentSize - start
	} else {
		fl = BATCH_SIZE - offset - 4
	}
	binary.BigEndian.PutUint32(buffer[offset:], uint32(fl))
	offset += 4

	log.Println(offset)
	log.Println(fl)
	log.Println(len(content))

	copy(buffer[offset:], content[start:start+fl])
	offset += fl
	log.Println(offset)

	return offset, fl
}

func buildFakeDNSRequest(request []byte, startOffset int) ([]byte, error) {
	buffer := make([]byte, 65535)
	bufferLength, fragmentSize := fakeDNSRequestEncode(buffer, request, startOffset)
	log.Printf("bufferLength: %d, fragmentSize: %d\n", bufferLength, fragmentSize)

	return buffer[:bufferLength], nil
}

func getHostFromFirstRequestBuffer(buffer []byte) []byte {
	secondLinePos := 0
	for i := 0; i < len(buffer); i++ {
		if buffer[i] == '\n' {
			secondLinePos = i + 1
			break
		}
	}

	if secondLinePos == 0 {
		return nil
	}

	hostStartPos := 0
	for i := secondLinePos; i < len(buffer); i++ {
		if buffer[i] == ' ' {
			hostStartPos = i + 1
		}
	}

	for i := hostStartPos; i < len(buffer); i++ {
		if buffer[i] == '\n' {
			return buffer[hostStartPos:i]
		}
	}

	return nil
}

func removeHostFromUrl(buffer []byte, host []byte) []byte {
	urlStartPos := 0
	for i := 0; i < len(buffer); i++ {
		if buffer[i] == ' ' {
			urlStartPos = i + 1
			break
		}
	}

	hostStartPos := 0
	for i := urlStartPos; i < len(buffer); i++ {
		if buffer[i] == ':' {
			hostStartPos = i + 3
		}
	}

	return append(buffer[:urlStartPos], buffer[hostStartPos+len(host):]...)
}

func receiveDNSResponseAndWriteback(closed *bool, conn *net.TCPConn, dnsconn *net.UDPConn) {
	buffer := make([]byte, 65535)
	for {
		// TODO read timeout
		n, err := dnsconn.Read(buffer)
		if err != nil {
			log.Printf("read dns response error: %s\n", err.Error())
			*closed = true
			return
		}
		if n < 4 {
			log.Println("read dns response error: length smaller than 4")
			*closed = true
			return
		}
		if binary.BigEndian.Uint32(buffer) == 0 {
			// http server closed connection
			*closed = true
			return
		}
		n, err = conn.Write(buffer[4:n])
		if err != nil {
			log.Printf("write back response to client error: %s\n", err.Error())
			*closed = true
			return
		}
	}
}

/***
1. read original request(stream) from client and save as []byte
2. build fake dns request
3. send fake dns request to dns server
4. receive dns resonse which is actually http response
5. write http response to client
***/
func processWholeProxyLife(conn *net.TCPConn) error {
	defer conn.Close()

	dnsconn, err := net.DialUDP("udp", nil, dnsServerAddr)
	if err != nil {
		return fmt.Errorf("could not dial to dns server[%s]: %s", dnsServerAddr, err)
	}
	log.Printf("connected to %s\n", dnsServerAddr)
	closed := false
	go receiveDNSResponseAndWriteback(&closed, conn, dnsconn)
	defer dnsconn.Close()

	//var host []byte
	//host = nil
	buffer := make([]byte, BATCH_SIZE)
	startOffset := 0
	for closed != true {
		n, err := conn.Read(buffer)
		if err != nil {
			return fmt.Errorf("could not read request from client: %s", err)
		}
		log.Printf("read %s bytes from client: %s\n", n, string(buffer[:n]))

		//if host == nil {
		//host := getHostFromFirstRequestBuffer(buffer)
		//if host == nil {
		//return fmt.Errorf("could not get host from first request buffer(length %d)\n", n)
		//}
		//buffer := removeHostFromUrl(buffer[:n], host)
		//n = len(buffer)
		//}

		fakeDNSRequest, err := buildFakeDNSRequest(buffer[:n], startOffset)
		if err != nil {
			return fmt.Errorf("could not build fake dns request: %s", err)
		}
		log.Printf("fake dns request built: %v\n", fakeDNSRequest)

		n, err = dnsconn.Write(fakeDNSRequest)
		if err != nil {
			return fmt.Errorf("could not send fake dns request: %s\n", err)
		}
		log.Printf("write %d bytes dns request to dns server\n", n)

		startOffset += n
	}

	return nil
}

func main() {
	tcpAddr, err := net.ResolveTCPAddr("tcp", options.listen)
	if err != nil {
		log.Fatalf("could not resolve tcp adress[%s]: %s\n", options.listen, err)
	}
	tcpListener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("could not listen on tcp adress[%s]: %s\n", options.listen, err)
	}

	log.Printf("listen on tcp adress[%s]\n", options.listen)

	for {
		conn, err := tcpListener.AcceptTCP()
		if err != nil {
			log.Printf("accept error: %s\n", err)
		}
		log.Printf("accept new conn[%s]", conn.RemoteAddr())
		go processWholeProxyLife(conn)
	}
}
