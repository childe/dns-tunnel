package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang/glog"
)

var hostRegexp = regexp.MustCompile(":\\d+$")

var BATCH_SIZE = 500

type requestFragments struct {
	cureentSize      int
	totalSize        int
	requests         [][]byte
	assembledRequest []byte
	lastUpdate       time.Time
	addr             net.Addr
}

var requestFragmentsMap map[string]*requestFragments

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
}

func cleanFragments() {
	for {
		if glog.V(5) {
			glog.Infof("clean expired fragments. requestFragmentsMap length : %d", len(requestFragmentsMap))
		}
		for client, fragments := range requestFragmentsMap {
			if glog.V(5) {
				glog.Infof("client[%s] last update at %s", client, fragments.lastUpdate)
			}
			if fragments.lastUpdate.Add(time.Second * time.Duration(options.expire)).Before(time.Now()) {
				glog.Infof("delete client[%s] from map", client)
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
	var offset int

	headers = make(map[string]string)

	urlstart := 0
	for offset := 0; offset < len(request); offset++ {
		if request[offset] == ' ' {
			if urlstart == 0 {
				method = string(request[:offset])
				glog.V(10).Infof("method:%s\n", method)
				urlstart = 1 + offset
			} else {
				url = string(request[urlstart:offset])
				glog.V(10).Infof("url:%s\n", url)
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
			headers[string(request[headStart:headEnd])] = string(request[headEnd+2 : offset])

			if request[offset+2] == '\r' && request[offset+3] == '\n' {
				body = request[offset+4:]
				glog.V(10).Infof("headers: %v", headers)
				return
			}

			offset += 2
			headStart = offset
		}
	}
	return
}

func readResponse(response *http.Response) (rst []byte) {
	//TODO return error
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
			glog.Errorf("read response body error: %s", err.Error())
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

func processRealRequest(rawrequest []byte) (*http.Response, error) {
	method, url, headers, body := readRequest(rawrequest)
	glog.V(2).Infof("launchHTTPRequest %s %s", method, url)
	if glog.V(5) {
		glog.Infof("headers: %v", headers)
		glog.Infof("body: %s", string(body))
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		request.Header[k] = []string{v}
	}

	client := &http.Client{}
	return client.Do(request)
}

func writebackResponse(buffer []byte, client net.Addr) error {
	if glog.V(10) {
		glog.Infoln(string(buffer))
		glog.Infof("response length: %d\n", len(buffer))
	}
	start := 0
	lengthBuffer := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuffer, uint32(len(buffer)))
	n, err := conn.WriteTo(lengthBuffer, client)
	if err != nil {
		return fmt.Errorf("could not write lengthBuffer to proxy client: %s", err.Error())
	}
	if n != 4 {
		return fmt.Errorf("did not fully write lengthBuffer to proxy client", err.Error())
	}

	glog.V(9).Infoln("write content length to proxy client")

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
			return fmt.Errorf("could not write batch to proxy client: %s", err.Error())
		}
		if glog.V(9) {
			glog.Infof("write back %d bytes response to proxy client", n)
		}
		if end == len(buffer) {
			return nil
		}
		start += n - 4
	}
	return nil
}

func processFragments() {
	for {
		for client, fragments := range requestFragmentsMap {
			if glog.V(9) {
				glog.Infof("process client[%s]\n. unprocessed requests length: %d", client, len(fragments.requests))
			}
			for _, request := range fragments.requests {
				totalSize, startPositon, fragmentSize, realContent := abstractRealContentFromRawRequest(request)
				if len(fragments.assembledRequest) == 0 {
					fragments.assembledRequest = make([]byte, totalSize)
				}
				if glog.V(9) {
					glog.Infof("totalSize:%d startPositon:%d fragmentSize:%d", totalSize, startPositon, fragmentSize)
					glog.Infof("realContent:%s", string(realContent))
				}
				copy(fragments.assembledRequest[startPositon:], realContent)
				fragments.cureentSize += fragmentSize
				// TODO
				fragments.totalSize = totalSize
			}
			if glog.V(9) {
				glog.Infof("total size: %d. cureent size:%d", fragments.totalSize, fragments.cureentSize)
			}
			fragments.requests = [][]byte{}
			if fragments.cureentSize == fragments.totalSize {
				glog.V(5).Infof("got fully message from %s", client)
				response, err := processRealRequest(fragments.assembledRequest)
				var writeErr error
				if err != nil {
					c := err.Error()
					writeErr = writebackResponse([]byte(c), fragments.addr)
				} else {
					writeErr = writebackResponse(readResponse(response), fragments.addr)
				}
				if writeErr != nil {
					glog.Errorf("write response back to %s error: %s", client, writeErr)
				} else {
					glog.V(5).Infof("write response back to %s", client)
				}
				glog.V(5).Infof("requests in client[%s] is being deleted", client)
				delete(requestFragmentsMap, client)
			}
		}
		time.Sleep(time.Duration(1000) * time.Millisecond)
	}
}

func main() {
	requestFragmentsMap = make(map[string]*requestFragments)
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
			glog.Errorf("read request error: %s", err)
		}
		if glog.V(9) {
			glog.Infof("read request. length: %d", n)
		}
		if value, ok := requestFragmentsMap[c.String()]; ok {
			if glog.V(5) {
				glog.Infof("client[%s] has been in map\n", c.String())
			}
			value.lastUpdate = time.Now()
			value.requests = append(value.requests, request[:n])
		} else {
			if glog.V(5) {
				glog.Infof("client[%s] has not been in map\n", c.String())
			}
			requests := [][]byte{request[:n]}
			requestFragmentsMap[c.String()] = &requestFragments{
				lastUpdate:       time.Now(),
				requests:         requests,
				assembledRequest: make([]byte, 0),
				cureentSize:      0,
				addr:             c,
			}
		}
	}
}
