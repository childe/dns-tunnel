package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var BATCH_SIZE = 500

var OUTOFMAXSIZE = errors.New("OUTOFMAXSIZE")

var options = &struct {
	host      string
	port      int
	dnsServer string
	domain    string
	timeout   uint64
}{}

var dnsServerAddr *net.UDPAddr

func init() {
	flag.StringVar(&options.host, "host", "127.0.0.1", "ip/host that bind to, default 127.0.0.1")
	flag.IntVar(&options.port, "port", 8080, "port that bind to, default 8080")
	flag.StringVar(&options.dnsServer, "dns", "", "dns server")
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

func getHeadersAndContentFromRawResponse(response []byte) (map[string]string, []byte) {
	lineStart := 0
	for i := 0; i < len(response); i++ {
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
			return headers, response[lineStart+2:]
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
	return nil, nil
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
	log.Printf("the full response is %d bytes\n", length)

	response := make([]byte, length)
	responseSize := 0
	buffer := make([]byte, 65535)
	for {
		//conn.SetReadDeadline(time.Now().Add(time.Millisecond * time.Duration(options.timeout)))
		n, err := conn.Read(buffer)
		if err != nil {
			return nil, err
		}
		log.Printf("read %d bytes from proxy server\n", n)
		offset := int(binary.BigEndian.Uint32(buffer[:4]))
		log.Printf("offset %d\n", offset)
		copy(response[offset:], buffer[4:n])
		responseSize += n - 4
		log.Printf("now response is %d bytes long\n", responseSize)
		if responseSize == length {
			return response, nil
		}
		if responseSize > length {
			return nil, errors.New("response received from proxy server is larger than it should be")
		}
	}
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	rawRequest, err := getStreamFromRequest(r)
	if err != nil {
		log.Printf("failed to get raw string from request: %s\n", err)
		w.Write([]byte("failed to get raw string from request.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}
	log.Println(string(rawRequest))
	log.Printf("rawRequest length: %d\n", len(rawRequest))

	conn, err := net.DialUDP("udp", nil, dnsServerAddr)
	if err != nil {
		log.Printf("could not connect to dns server: %s\n", err)
		w.Write([]byte("could not connect to dns server.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}
	log.Printf("connected to %s\n", dnsServerAddr)
	defer conn.Close()

	start := 0
	for {
		buffer := make([]byte, BATCH_SIZE)
		bufferLength, fragmentSize := fakeDNSRequestEncode(buffer, rawRequest, start)
		log.Printf("fake dns request length: %d\n", bufferLength)
		conn.Write(buffer[:bufferLength])
		if start+fragmentSize >= len(rawRequest) {
			break
		} else {
			start += fragmentSize
		}
	}

	response, err := getResponse(conn)
	if err != nil {
		log.Printf("read fake dns response error: %s\n", err)
		w.Write([]byte("read fake dns response error.\n"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}

	for key, _ := range w.Header() {
		w.Header().Del(key)
	}
	headers, content := getHeadersAndContentFromRawResponse(response)
	log.Printf("content length: %d\n", len(content))
	for key, v := range headers {
		w.Header().Set(key, v)
	}
	w.Write(content)
	w.WriteHeader(200)
	return
}

// the totalSize|startPositon|FragmentSize Protocol
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

	copy(buffer[offset:], content[start:start+fl])
	offset += fl

	return offset, fl
}

func main() {
	http.HandleFunc("/", proxyHandler)
	log.Printf("%s:%d", options.host, options.port)
	http.ListenAndServe(fmt.Sprintf("%s:%d", options.host, options.port), nil)
}
