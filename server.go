package main

import (
	"encoding/binary"
	"flag"
	"fmt"
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
	udpBatchSize  int
}{}

func init() {
	flag.StringVar(&options.bind, "bind", "0.0.0.0:53", "to which address faked dns server bind")
	flag.IntVar(&options.cleanInterval, "clean-interval", 60, "")
	flag.IntVar(&options.expire, "expire", 300, "expire time (s)")
	flag.IntVar(&options.udpBatchSize, "udp-batch-size", 4096, "udp package could not be too long")
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
	log.Println(string(buffer))
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
		log.Println(string(bs[:n]))
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
	// RFC 2616: Must treat
	//	GET /index.html HTTP/1.1
	//	Host: www.google.com
	// and
	//	GET http://www.google.com/index.html HTTP/1.1
	//	Host: doesntmatter
	// the same. In the second case, any Host line is ignored.
	return request
	//urlstart := 0
	//for i := 0; i < len(request); i++ {
	//if request[i] == ' ' {
	//urlstart = i + 1
	//break
	//}
	//}

	//var pathstart int
	//for i := urlstart; i < len(request); i++ {
	//if request[i] == ':' {
	//pathstart += i + 3 + len(host)
	//break
	//}
	//}

	//return append(request[:urlstart], request[pathstart:]...)
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

func writebackResponse(buffer []byte, client net.Addr) error {
	log.Println(string(buffer))
	log.Printf("response length: %d\n", len(buffer))
	start := 0
	lengthBuffer := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuffer, uint32(len(buffer)))
	n, err := conn.WriteTo(lengthBuffer, client)
	if err != nil {
		return fmt.Errorf("could not write lengthBuffer to proxy client: %s\n", err.Error())
	}
	if n != 4 {
		return fmt.Errorf("did not fully write lengthBuffer to proxy client\n", err.Error())
	}

	for {
		end := start + options.udpBatchSize
		if end > len(buffer) {
			end = len(buffer)
		}
		batch := make([]byte, 4+end-start)
		binary.BigEndian.PutUint32(batch, uint32(start))
		copy(batch[4:], buffer[start:end])
		n, err := conn.WriteTo(batch, client)
		if err != nil {
			return fmt.Errorf("could not write batch to proxy client: %s\n", err.Error())
		}
		log.Printf("write back %d bytes response to proxy client\n", n)
		if end == len(buffer) {
			return nil
		}
		start += n - 4
	}
	return nil
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
				if err != nil {
					c := err.Error()
					writeErr = writebackResponse([]byte(c), client)
				} else {
					writeErr = writebackResponse(response, client)
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
