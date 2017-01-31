package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
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

	var err error
	dnsServerAddr, err = net.ResolveUDPAddr("udp", options.dnsServer)
	if err != nil {
		panic(err)
	}
	glog.Infoln("resolved dns server address")
}

// Protocol: totalSize|startPositon|FragmentSize
func getStreamFromRequest(r *http.Request) ([]byte, error) {
	rst := make([]byte, 0)
	//TODO
	rst = append(rst, fmt.Sprintf("%s %s %s\n", r.Method, r.URL.String(), r.Proto)...)
	rst = append(rst, fmt.Sprintf("Host: %s\n", r.Host)...)
	for k, v := range r.Header {
		rst = append(rst, fmt.Sprintf("%s: %s\n", k, strings.Join(v, ","))...)
	}

	rst = append(rst, '\n')

	p := make([]byte, 1024)
	for {
		n, err := r.Body.Read(p)
		if err != nil && err != io.EOF {
			return nil, err
		}
		rst = append(rst, p[:n]...)
		if err == io.EOF {
			break
		}
	}
	return rst, nil
}

func getHeadersCodeContentFromRawResponse(response []byte) (int, map[string]string, []byte) {
	lineStart := 0
	codestart := 0
	var code int
	var err error
	for i := 0; i < len(response); i++ {
		if response[i] == ' ' {
			if codestart == 0 {
				codestart = i + 1
			} else if codestart != -1 {
				code, err = strconv.Atoi((string(response[codestart:i])))
				if err != nil {
					glog.Errorf("could not convert \"%s\" to code: %s", string(response[codestart:i]), err)
					return 0, nil, nil
				}
				codestart = -1
			}
		}
		if response[i] == '\r' && response[i+1] == '\n' {
			lineStart = i + 2
			break
		}
	}

	headers := make(map[string]string)
	var key string
	var valueStart int
	for {
		if response[lineStart] == '\r' && response[lineStart+1] == '\n' {
			return code, headers, response[lineStart+2:]
		}

		for i := lineStart; i < len(response); i++ {
			if response[i] == ':' && response[i+1] == ' ' {
				key = string(response[lineStart:i])
				valueStart = i + 2
				break
			}
		}

		for i := valueStart; i < len(response); i++ {
			if response[i] == '\r' && response[i+1] == '\n' {
				headers[key] = string(response[valueStart:i])
				lineStart = i + 2
				break
			}
		}
	}
	return code, nil, nil
}

func getResponse(conn *net.UDPConn) ([]byte, error) {
	lengthBuffer := make([]byte, 4)
	//conn.SetReadDeadline(time.Now().Add(time.Millisecond * time.Duration(options.timeout)))
	n, err := conn.Read(lengthBuffer)
	if err != nil {
		return nil, err
	}

	if n != 4 {
		return nil, fmt.Errorf("could not read full lengthResponse")
	}

	length := int(binary.BigEndian.Uint32(lengthBuffer))
	if glog.V(5) {
		glog.Infof("the full response is %d bytes", length)
	}

	response := make([]byte, length)
	responseSize := 0
	buffer := make([]byte, 65535)
	for {
		//conn.SetReadDeadline(time.Now().Add(time.Millisecond * time.Duration(options.timeout)))
		n, err := conn.Read(buffer)
		if err != nil {
			return nil, err
		}
		if glog.V(9) {
			glog.Infof("read %d bytes from proxy server", n)
		}

		offset := int(binary.BigEndian.Uint32(buffer[:4]))
		if glog.V(9) {
			glog.Infof("offset %d", offset)
		}

		copy(response[offset:], buffer[4:n])
		responseSize += n - 4
		if glog.V(9) {
			glog.Infof("now response is %d bytes long", responseSize)
		}

		if responseSize == length {
			return response, nil
		}
		if responseSize > length {
			return nil, errors.New("response received from proxy server is larger than it should be")
		}
	}
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	glog.Infof("%s %s", r.Method, r.URL)
	rawRequest, err := getStreamFromRequest(r)
	if glog.V(10) {
		glog.Infoln(rawRequest)
		glog.Infof("rawRequest length: %d", len(rawRequest))
	}

	if err != nil {
		glog.Errorf("failed to get raw string from request: %s", err)
		w.Write([]byte("failed to get raw string from request.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}

	conn, err := net.DialUDP("udp", nil, dnsServerAddr)
	defer conn.Close()
	glog.Infof("%s %s %s", conn.LocalAddr(), r.Method, r.URL)

	if err != nil {
		glog.Infof("could not connect to dns server: %s\n", err)
		w.Write([]byte("could not connect to dns server.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}
	if glog.V(5) {
		glog.Infof("connected to %s", dnsServerAddr)
	}

	start := 0
	for {
		buffer := make([]byte, BATCH_SIZE)
		bufferLength, fragmentSize := fakeDNSRequestEncode(buffer, rawRequest, start)
		glog.V(9).Infof("fake dns request length: %d", bufferLength)
		conn.Write(buffer[:bufferLength])
		if start+fragmentSize >= len(rawRequest) {
			break
		} else {
			start += fragmentSize
		}
	}

	response, err := getResponse(conn)
	if err != nil {
		glog.Errorf("read fake dns response error: %s", err)
		w.Write([]byte("read fake dns response error.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	} else {
		glog.V(9).Infof("got response[%d bytes] from proxy server", len(response))
	}

	for key, _ := range w.Header() {
		w.Header().Del(key)
	}

	code, headers, content := getHeadersCodeContentFromRawResponse(response)
	glog.Infof("response from proxy server: %s code: %d. content length: %d", conn.LocalAddr(), code, len(content))

	for key, v := range headers {
		w.Header().Set(key, v)
	}
	w.Header().Set("ProxyAgent", "dnstunnel")
	if glog.V(9) {
		glog.Infof("processed response headers: %v\n", w.Header())
	}

	w.WriteHeader(code)
	w.Write(content)
	return
}

// the totalSize|startPositon|FragmentSize Protocol
func fakeDNSRequestEncode(buffer, content []byte, start int) (int, int) {
	if glog.V(9) {
		glog.Infof("fakeDNSRequestEncode buffer size: %d", len(buffer))
		glog.Infof("fakeDNSRequestEncode start postion: %d", start)
	}

	now := time.Now().UnixNano()
	domain := fmt.Sprintf("%d.%s", now, options.domain)

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

	copy(buffer[offset:], content[start:start+fl])
	offset += fl

	return offset, fl
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

func main() {
	http.HandleFunc("/", proxyHandler)
	http.ListenAndServe(options.listen, nil)
}
