package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
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

// read request and return headers and request body
func readRequest(request []byte) (method, url string, headers map[string]string, body []byte) {
	log.Println("readRequest func")
	var offset int

	headers = make(map[string]string)

	urlstart := 0
	for offset := 0; offset < len(request); offset++ {
		//log.Printf("offset:%d %c\n", offset, request[offset])
		if request[offset] == ' ' {
			if urlstart == 0 {
				method = string(request[:offset])
				log.Printf("method:%s\n", method)
				urlstart = 1 + offset
			} else {
				url = string(request[urlstart:offset])
				log.Printf("url:%s\n", url)
				break
			}
		}
	}

	// read until first line break
	for ; offset < len(request); offset++ {
		if request[offset] == '\r' && request[offset+1] == '\n' {
			offset += 2
			break
		}
	}

	var headStart, headEnd int
	headStart = offset
	for ; offset < len(request); offset++ {
		if request[offset] == ':' && request[offset+1] == ' ' {
			headEnd = offset
			offset += 2
			continue
		}
		if request[offset] == '\r' && request[offset+1] == '\n' {
			log.Printf("offset: %d\theadStart: %d\theadEnd:%d\n", offset, headStart, headEnd)
			headers[string(request[headStart:headEnd])] = string(request[headEnd+2 : offset])

			if request[offset+2] == '\r' && request[offset+3] == '\n' {
				body = request[offset+4:]
				log.Println(headers)
				return
			}

			offset += 2
			headStart = offset
		}
	}
	log.Println(headers)
	return
}

func launchHTTPRequest(method, url string, headers map[string]string, body []byte) (*http.Response, error) {
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	log.Printf("original http request header: %v\n", request.Header)
	for k, v := range headers {
		request.Header[k] = []string{v}
	}
	log.Printf("http request header after reset: %v\n", request.Header)

	client := &http.Client{}
	return client.Do(request)
}

func readResponse(response *http.Response) (rst []byte) {
	log.Printf("response header: %v\n", response.Header)
	rst = append(rst, response.Proto...)
	rst = append(rst, ' ')
	rst = append(rst, response.Status...)
	rst = append(rst, '\r', '\n')
	for k, v := range response.Header {
		rst = append(rst, k...)
		rst = append(rst, ':', ' ')
		rst = append(rst, strings.Join(v, ",")...)
		rst = append(rst, '\r', '\n')
	}
	rst = append(rst, '\r', '\n')
	buffer := make([]byte, 65535)
	for {
		n, err := response.Body.Read(buffer)
		rst = append(rst, buffer[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("read response body error: %s\n", err.Error())
			return nil
		}
	}
	return
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

func processRealRequest(request []byte) (*http.Response, error) {
	method, url, headers, body := readRequest(request)
	log.Printf("method: %s\n", method)
	log.Printf("url: %s\n", url)
	log.Printf("headers: %v\n", headers)
	log.Printf("body: %s\n", body)
	return launchHTTPRequest(method, url, headers, body)
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

	log.Println("write content length to proxy client")

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
			break
			for _, request := range fragments.requests {
				totalSize, startPositon, fragmentSize, realContent := abstractRealContentFromRawRequest(request)
				if len(fragments.assembledRequest) == 0 {
					fragments.assembledRequest = make([]byte, totalSize)
				}
				log.Printf("totalSize:%d startPositon:%d fragmentSize:%d\n", totalSize, startPositon, fragmentSize)
				log.Printf("realContent:%s\n", string(realContent))
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
				if err != nil {
					c := err.Error()
					writeErr = writebackResponse([]byte(c), client)
				} else {
					writeErr = writebackResponse(readResponse(response), client)
				}
				if writeErr != nil {
					log.Printf("write response back error: %s\n", writeErr)
				} else {
					log.Printf("write response back\n")
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
