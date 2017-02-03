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

type clientBlock struct {
	lastUpdate       time.Time
	addr             *net.UDPAddr
	originalRequests [][]byte
	requestSlices    map[uint32][]byte
	nextOffset       uint32
	host             []byte
	conn             *net.TCPConn
}

var clientBlocksMap map[string]*clientBlock = make(map[string]*clientBlock)

var DNSconn *net.UDPConn

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
			glog.Infof("clean expired fragments. requestFragmentsMap length : %d", len(clientBlocksMap))
		}
		for client, fragments := range clientBlocksMap {
			if glog.V(5) {
				glog.Infof("client[%s] last update at %s", client, fragments.lastUpdate)
			}
			if fragments.lastUpdate.Add(time.Second * time.Duration(options.expire)).Before(time.Now()) {
				glog.Infof("delete client[%s] from map", client)
				delete(clientBlocksMap, client)
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

func processRealRequest(rawrequest []byte) (*http.Response, error) {
	method, url, headers, body := readRequest(rawrequest)
	glog.V(2).Infof("launchHTTPRequest %s %s", method, url)
	if glog.V(5) {
		glog.Infof("headers: %v", headers)
		glog.Infof("body: %s", body)
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
		glog.Infof("response length: %d", len(buffer))
	}
	start := 0
	lengthBuffer := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuffer, uint32(len(buffer)))
	n, err := DNSconn.WriteTo(lengthBuffer, client)
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
		n, err := DNSconn.WriteTo(batch, client)
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

func connectHost(host []byte) *net.TCPConn {
	if !hostRegexp.Match(host) {
		host = append(host, ":80"...)
	}
	serverAddr, err := net.ResolveTCPAddr("tcp", string(host))
	if err != nil {
		glog.Errorf("could not resove address[%s]", host)
		return nil
	}

	tcpConn, err := net.DialTCP("tcp", nil, serverAddr)
	if err != nil {
		glog.Errorf("could not dial to address[%s]", host)
		return nil
	}
	return tcpConn
}

func sendBuffer(conn *net.TCPConn, buffer []byte) error {
	for {
		n, err := conn.Write(buffer)
		if err != nil {
			return err
		}

		if n == len(buffer) {
			return nil
		}
		buffer = buffer[n:]
	}
}

func passBetweenRealServerAndProxyClient(conn *net.TCPConn, dnsRemoteAddr *net.UDPAddr) {
	buffer := make([]byte, options.udpBatchSize)
	for {
		n, err := conn.Read(buffer[4:])
		if err == io.EOF {
			conn.Close()
			DNSconn.Write([]byte{0, 0, 0, 0})
			return
		}
		if err != nil {
			glog.Errorf("read from real server error:%s", err)
		}
		binary.BigEndian.PutUint32(buffer, uint32(n))
		DNSconn.Write(buffer[:4+n])
	}
}

/*
parse host and build connection;
loop requestSlices and send to real server
*/
func processClientRequest(client string) {
	glog.V(9).Infof("process client %s", client)
	clientBlock, _ := clientBlocksMap[client]
	glog.V(9).Infof("client[%s] there is %d original requests", client, len(clientBlock.originalRequests))

	for idx, originalRequest := range clientBlock.originalRequests {
		glog.V(9).Infof("client[%s] process the %d original request", client, idx)
		offset := binary.BigEndian.Uint32(originalRequest[:4])
		glog.V(9).Infof("client[%s] offset %d", client, offset)
		if offset == 0 {
			hostLength := binary.BigEndian.Uint32(originalRequest[4:8])
			clientBlock.host = originalRequest[8 : hostLength+8]

			glog.V(9).Infof("got host[%s] in client %s", clientBlock.host, client)

			clientBlock.requestSlices[offset] = originalRequest[8+hostLength:]

			conn := connectHost(clientBlock.host)
			if conn != nil {
				clientBlock.conn = conn
				go passBetweenRealServerAndProxyClient(conn, clientBlock.addr)
			} else {
				//TODO lock?
				delete(clientBlocksMap, client)
				return
			}
		} else {
			clientBlock.requestSlices[offset] = originalRequest[4:]
		}
	}

	//TODO lock?
	clientBlock.originalRequests = make([][]byte, 0)

	if clientBlock.conn == nil {
		// Not get host yet
		return
	}

	changed := true
	for changed == true {
		changed = false
		for offset, content := range clientBlock.requestSlices {
			if offset == clientBlock.nextOffset {
				err := sendBuffer(clientBlock.conn, content)
				if err != nil {
					//TODO if conn has been closed ?
					glog.Errorf("could not send request to real server[%s]: %s", clientBlock.conn.RemoteAddr(), err)
				} else {
					//TODO delete in a loop??
					delete(clientBlock.requestSlices, offset)
					clientBlock.nextOffset += uint32(len(content))
					changed = true
				}
			}
		}
	}
}

func main() {
	udpAddr, err := net.ResolveUDPAddr("udp", options.bind)
	if err != nil {
		panic(err)
	}

	DNSconn, err = net.ListenUDP("udp", udpAddr)

	if err != nil {
		panic(err)
	}
	glog.Infof("listen on %s", options.bind)

	go cleanFragments()

	for {
		request := make([]byte, 65535)
		n, c, err := DNSconn.ReadFromUDP(request)
		if err != nil {
			glog.Errorf("read request from %s error: %s", c, err)
			continue
		}

		if glog.V(9) {
			glog.Infof("read request from %s, length: %d", c, n)
		}

		if value, ok := clientBlocksMap[c.String()]; ok {
			if glog.V(5) {
				glog.Infof("client[%s] has been in map", c)
			}
			value.lastUpdate = time.Now()
			value.originalRequests = append(value.originalRequests, request[:n])
		} else {
			if glog.V(5) {
				glog.Infof("client[%s] has not been in map", c)
			}
			originalRequests := [][]byte{request[:n]}
			clientBlocksMap[c.String()] = &clientBlock{
				lastUpdate:       time.Now(),
				originalRequests: originalRequests,
				requestSlices:    map[uint32][]byte{},
				nextOffset:       0,
				addr:             c,
				host:             nil,
				conn:             nil,
			}
		}
		go processClientRequest(c.String())
	}
}
