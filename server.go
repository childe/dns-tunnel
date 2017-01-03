package main

import (
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"regexp"
	"time"
)

var hostRegexp = regexp.MustCompile(":\\d+$")

var BATCH_SIZE = 500

type requestFragments struct {
	cureentSize      int
	totalSize        int
	requests         [][]byte
	assembledRequest []byte
	lastUpdate       time.Time
}

var requestFragmentsMap map[net.Addr]*requestFragments

var conn *net.UDPConn

var options = &struct {
	bind          string
	cleanInterval int
	expire        int
}{}

func init() {
	flag.StringVar(&options.bind, "bind", "0.0.0.0:53", "to which address faked dns server bind")
	flag.IntVar(&options.cleanInterval, "clean-interval", 60, "")
	flag.IntVar(&options.expire, "expire", 300, "expire time (s)")
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func getResponseForClient(c net.Addr, buffer []byte) {
	n, err := conn.WriteTo(buffer, c)
	if err != nil {
		log.Println(err)
	}
	log.Println(n)
}

func cleanFragments() {
	for {
		log.Printf("clean expired fragments. requestFragmentsMap length : %d\n", len(requestFragmentsMap))
		for client, fragments := range requestFragmentsMap {
			log.Printf("client[%s] last update at %s\n", client, fragments.lastUpdate)
			if fragments.lastUpdate.Add(time.Second * time.Duration(options.expire)).Before(time.Now()) {
				log.Printf("delete client[%s] from map\n", client)
				delete(requestFragmentsMap, client)
			}
		}
		time.Sleep(time.Duration(options.cleanInterval) * time.Second)
	}
}

func getHostFromRealRequest(request []byte) string {
	start := 0
	for i := 0; i < len(request); i++ {
		if request[i] == '\n' {
			if start == 0 {
				start = i
			} else {
				return string(request[7+start : i])
			}
		}
	}
	return ""
}

func abstractRealContentFromRawRequest(rawRequest []byte) (int, int, int, []byte) {
	questionNumber := int(binary.BigEndian.Uint16(rawRequest[4:]))
	offset := 12
	for i := 0; i < questionNumber; i++ {
		for {
			byteCount := int(rawRequest[offset])
			if byteCount == 0 {
				offset++
				break
			}
			offset += 1 + byteCount
		}
	}
	offset += 4
	totalSize := int(binary.BigEndian.Uint32(rawRequest[offset:]))
	startPositon := int(binary.BigEndian.Uint32(rawRequest[offset+4:]))
	fragmentSize := int(binary.BigEndian.Uint32(rawRequest[offset+8:]))
	return totalSize, startPositon, fragmentSize, rawRequest[offset+12:]
}

func launchHTTPRequest(host string, buffer []byte) ([]byte, error) {
	if !hostRegexp.Match([]byte(host)) {
		host += ":80"
	}
	log.Printf("host with port: %s\n", host)
	tcpAddr, err := net.ResolveTCPAddr("tcp", host)
	if err != nil {
		log.Printf("failed to resolve tcp address from %s: %s\n", host, err)
		return nil, err
	} else {
		log.Printf("tcp address to %s resolved\n", host)
	}
	tcpConn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		log.Printf("failed to get tcp connection: %s\n", host, err)
		return nil, err
	} else {
		log.Printf("tcp connection to %s built\n", host)
	}
	defer tcpConn.Close()
	_, err = tcpConn.Write(buffer)
	if err != nil {
		log.Printf("failed to write request to host %s: %s\n", host, err)
		return nil, err
	} else {
		log.Println("write request to host")
	}
	response := []byte{}
	bs := make([]byte, 4096)
	for {
		n, err := tcpConn.Read(bs)
		log.Printf("read %d bytes from http server\n", n)
		if err != nil && err != io.EOF {
			log.Printf("faild to read from http server: %s\n", err)
			return nil, err
		}
		response = append(response, bs[:n]...)
		if err == io.EOF {
			return response, nil
		}
	}
	return response, nil
}

func removeHostFromUrl(request []byte, host string) []byte {
	urlstart := 0
	for i := 0; i < len(request); i++ {
		if request[i] == ' ' {
			urlstart = i + 1
			break
		}
	}

	var pathstart int
	for i := urlstart; i < len(request); i++ {
		if request[i] == ':' {
			pathstart += i + 3 + len(host)
			break
		}
	}

	return append(request[:urlstart], request[pathstart:]...)
}

func processRealRequest(request []byte) ([]byte, error) {
	host := getHostFromRealRequest(request)
	log.Printf("host: %s\n", host)
	response, err := launchHTTPRequest(host, removeHostFromUrl(request, host))
	if err != nil {
		log.Printf("failed to get http response: %s\n", err)
		return nil, err
	}
	return response, nil
}

func processFragments() {
	for {
		//log.Printf("process fragments. requestFragmentsMap length : %d\n", len(requestFragmentsMap))
		for client, fragments := range requestFragmentsMap {
			log.Printf("process client[%s]\n", client)
			log.Printf("unprocessed requests length: %d\n", len(fragments.requests))
			for _, request := range fragments.requests {
				totalSize, startPositon, fragmentSize, realContent := abstractRealContentFromRawRequest(request)
				if len(fragments.assembledRequest) == 0 {
					fragments.assembledRequest = make([]byte, totalSize)
				}
				log.Printf("%d %d %d\n", totalSize, startPositon, fragmentSize)
				log.Println(string(realContent))
				copy(fragments.assembledRequest[startPositon:], realContent)
				fragments.cureentSize += fragmentSize
				// TODO
				fragments.totalSize = totalSize
			}
			log.Printf("total size: %d. cureent size:%d\n", fragments.totalSize, fragments.cureentSize)
			fragments.requests = [][]byte{}
			if fragments.cureentSize == fragments.totalSize {
				response, err := processRealRequest(fragments.assembledRequest)
				var writeErr error
				var n int
				lengthBuffer := make([]byte, 4)
				if err != nil {
					c := err.Error()
					binary.BigEndian.PutUint32(lengthBuffer, uint32(len(c)))
					conn.WriteTo(lengthBuffer, client)
					n, writeErr = conn.WriteTo([]byte(c), client)
				} else {
					binary.BigEndian.PutUint32(lengthBuffer, uint32(len(response)))
					conn.WriteTo(lengthBuffer, client)
					n, writeErr = conn.WriteTo(response, client)
				}
				if writeErr != nil {
					log.Printf("write response back error: %s\n", writeErr)
				} else {
					log.Printf("write %d bytes response back\n", n)
				}
				log.Printf("requests in client[%s] is being deleted\n", client)
				delete(requestFragmentsMap, client)
			}
		}
		time.Sleep(time.Duration(1000) * time.Millisecond)
	}
}

func main() {
	requestFragmentsMap = make(map[net.Addr]*requestFragments)
	udpAddr, err := net.ResolveUDPAddr("udp4", options.bind)
	if err != nil {
		panic(err)
	}

	conn, err = net.ListenUDP("udp4", udpAddr)

	if err != nil {
		panic(err)
	}

	go cleanFragments()
	go processFragments()

	for {
		request := make([]byte, BATCH_SIZE)
		n, c, err := conn.ReadFrom(request)
		if err != nil {
			log.Printf("read request error: %s\n", err)
		}
		log.Printf("read request. length: %d", n)
		if value, ok := requestFragmentsMap[c]; ok {
			log.Printf("client[%s] has been in map\n", c)
			value.lastUpdate = time.Now()
			value.requests = append(value.requests, request[:n])
		} else {
			log.Printf("client[%s] has not been in map\n", c)
			requests := [][]byte{request[:n]}
			requestFragmentsMap[c] = &requestFragments{
				lastUpdate:       time.Now(),
				requests:         requests,
				assembledRequest: make([]byte, 0),
				cureentSize:      0,
			}
		}
		log.Printf("requestFragmentsMap length : %d\n", len(requestFragmentsMap))
	}
}
