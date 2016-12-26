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

var MAX_SIZE = 65535
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

func getStreamFromRequest(r *http.Request) (string, error) {
	rst := ""
	rst += fmt.Sprintf("%s %s %s\n", r.Method, r.URL.String(), r.Proto)
	rst += fmt.Sprintf("Host: %s\n", r.Host)
	for k, v := range r.Header {
		rst += fmt.Sprintf("%s: %s\n", k, strings.Join(v, ","))
	}

	rst += "\n"

	p := make([]byte, MAX_SIZE)
	size := 0
	for {
		n, err := r.Body.Read(p)
		if err != nil && err != io.EOF {
			return "", err
		}
		rst += string(p[:n])
		size += n
		if err == io.EOF {
			break
		}
	}
	if size > MAX_SIZE {
		return "", OUTOFMAXSIZE
	}
	return rst, nil
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
	log.Println(rawRequest)

	buffer := make([]byte, MAX_SIZE)
	length := fakeDNSRequestEncode(rawRequest, buffer)

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

	conn.Write(buffer[:length])

	conn.SetReadDeadline(time.Now().Add(time.Millisecond * time.Duration(options.timeout)))
	n, err := conn.Read(buffer)
	if err != nil {
		log.Printf("read dns response timeout: %s\n", err)
		w.Write([]byte("read dns response timeout"))
		w.Write([]byte(err.Error()))
		w.WriteHeader(500)
		return
	}
	log.Printf("read %d bytes\n", n)

	w.WriteHeader(200)
	return
}

func fakeDNSRequestEncode(content string, buffer []byte) int {
	now := time.Now().Unix()
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

	copy(buffer[offset:], []byte(content))

	offset += len(content)

	return offset
}

func main() {
	http.HandleFunc("/", proxyHandler)
	log.Printf("%s:%d", options.host, options.port)
	http.ListenAndServe(fmt.Sprintf("%s:%d", options.host, options.port), nil)
}
