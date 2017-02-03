package main

import (
	"encoding/binary"
	"flag"
	"io"
	"net"
	"regexp"
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

func abstractRealContentFromRawRequest(rawRequest []byte) (uint32, []byte) {
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
	start := binary.BigEndian.Uint32(rawRequest[offset:])
	return start, rawRequest[offset+4:]
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
	glog.V(10).Infof("content sent to %s:%s", conn.RemoteAddr(), buffer)
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
	streamIdx := 1
	for {
		n, err := conn.Read(buffer[4:])
		if err == io.EOF {
			glog.V(5).Infof("connection[%s] closed from server", conn.RemoteAddr())
			conn.Close()
			DNSconn.Write([]byte{0, 0, 0, 0})
			return
		}
		if err != nil {
			glog.Errorf("read from real server error:%s", err)
			return
		}
		glog.V(9).Infof("connection[%s] read %d bytes response", conn.RemoteAddr(), n)
		binary.BigEndian.PutUint32(buffer, uint32(streamIdx))
		DNSconn.Write(buffer[:4+n])
		streamIdx++
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
		offset, content := abstractRealContentFromRawRequest(originalRequest)
		glog.V(9).Infof("client[%s] offset %d", client, offset)
		if offset == 0 {
			hostLength := binary.BigEndian.Uint32(content[:4])
			clientBlock.host = content[4 : hostLength+4]

			glog.V(5).Infof("client[%s] got host[%s]", client, clientBlock.host)

			clientBlock.requestSlices[offset] = content[4+hostLength:]

			conn := connectHost(clientBlock.host)
			if conn != nil {
				glog.V(5).Infof("client[%s] connected to host[%s]", client, clientBlock.host)
				clientBlock.conn = conn
				go passBetweenRealServerAndProxyClient(conn, clientBlock.addr)
			} else {
				//TODO lock?
				delete(clientBlocksMap, client)
				return
			}
		} else {
			clientBlock.requestSlices[offset] = content
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
					glog.V(9).Infof("client[%s] request slice sent to real server", client)
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
