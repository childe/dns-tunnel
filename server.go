package main

import (
	"encoding/binary"
	"flag"
	"io"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/golang/glog"
)

var hostRegexp = regexp.MustCompile(":\\d+$")
var cblockmu sync.Mutex
var DNSconn *net.UDPConn

type clientBlock struct {
	lastUpdate       time.Time
	addr             *net.UDPAddr
	originalRequests [][]byte
	requestSlices    map[uint32][]byte
	nextOffset       uint32
	host             []byte
	conn             *net.TCPConn
	mu               sync.Mutex
	co               sync.Cond
}

var clientBlocksMap map[string]*clientBlock = make(map[string]*clientBlock)

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
				fragments.co.Signal()
				glog.Infof("delete client[%s] from map", client)
				cblockmu.Lock()
				delete(clientBlocksMap, client)
				cblockmu.Unlock()
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
	return start, append([]byte{}, rawRequest[offset+4:]...)
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
	glog.V(9).Infof("content sent to %s:%s", conn.RemoteAddr(), buffer)
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
			glog.V(2).Infof("client[%s] connection[%s] closed from server", dnsRemoteAddr, conn.RemoteAddr())
			conn.Close()
			DNSconn.Write([]byte{0, 0, 0, 0})
			return
		}
		if err != nil {
			glog.Errorf("read from real server error:%s", err)
			return
		}
		glog.V(9).Infof("client[%s] connection[%s] read %d bytes response", dnsRemoteAddr, conn.RemoteAddr(), n)
		binary.BigEndian.PutUint32(buffer, uint32(streamIdx))
		_, err = DNSconn.WriteToUDP(buffer[:4+n], dnsRemoteAddr)
		if err != nil {
			glog.Errorf("client[%s] send to proxy client error:%s", dnsRemoteAddr, err)
		} else if glog.V(9) {
			glog.Infof("client[%s] send to proxy client with slice index %d", dnsRemoteAddr, streamIdx)
		}
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

	for {
		clientBlock.mu.Lock()
		if len(clientBlock.originalRequests) == 0 {
			clientBlock.mu.Unlock()
			//如果request slice在路上丢失, 可能会导致goroutine永远不醒来不return.
			//所以在clear expire的时候, 需要Signal
			clientBlock.co.Wait()
		}
		for idx, originalRequest := range clientBlock.originalRequests {
			glog.V(9).Infof("client[%s] process the %d original request", client, idx)
			offset, content := abstractRealContentFromRawRequest(originalRequest)
			glog.V(9).Infof("client[%s] offset %d", client, offset)
			if offset == 0 {
				hostLength := binary.BigEndian.Uint32(content[:4])
				clientBlock.host = append([]byte{}, content[4:hostLength+4]...)

				glog.Infof("client[%s] got host[%s]", client, clientBlock.host)

				clientBlock.requestSlices[offset] = content[4+hostLength:]

			} else {
				clientBlock.requestSlices[offset] = content
			}
		}

		clientBlock.originalRequests = make([][]byte, 0)
		clientBlock.mu.Unlock()

		if clientBlock.conn == nil {
			conn := connectHost(clientBlock.host)
			if conn != nil {
				glog.V(5).Infof("client[%s] connected to host[%s]", client, clientBlock.host)
				clientBlock.conn = conn
				go passBetweenRealServerAndProxyClient(conn, clientBlock.addr)
			} else {
				glog.Errorf("client[%s] could not connect to host[%s]", client, clientBlock.host)
				cblockmu.Lock()
				delete(clientBlocksMap, client)
				cblockmu.Unlock()
				return
			}
		}

		for {
			if content, ok := clientBlock.requestSlices[clientBlock.nextOffset]; ok {
				err := sendBuffer(clientBlock.conn, content)
				if err != nil {
					//TODO if conn has been closed ?
					glog.Errorf("could not send request to real server[%s]: %s", clientBlock.conn.RemoteAddr(), err)
				} else {
					glog.V(9).Infof("client[%s] request slice sent to real server", client)
					//TODO delete in a loop??
					delete(clientBlock.requestSlices, clientBlock.nextOffset)
					clientBlock.nextOffset += uint32(len(content))
				}
			} else {
				return
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

	buffer := make([]byte, 65535)
	for {
		n, c, err := DNSconn.ReadFromUDP(buffer)
		if err != nil {
			glog.Errorf("read from %s error: %s", c, err)
			continue
		}

		if glog.V(9) {
			glog.Infof("read %d bytes from %s", n, c)
		}

		request := make([]byte, n)
		copy(request, buffer[:n])

		cblockmu.Lock()
		// 琐应该放在最外面. 避免更新value的同时, 它也被删除
		if value, ok := clientBlocksMap[c.String()]; ok {
			if glog.V(5) {
				glog.Infof("client[%s] has been in map", c)
			}
			value.lastUpdate = time.Now()
			value.mu.Lock()
			value.originalRequests = append(value.originalRequests, request)
			value.mu.Unlock()
			value.co.Signal()
		} else {
			if glog.V(5) {
				glog.Infof("client[%s] is not in map", c)
			}
			originalRequests := [][]byte{request}
			clientBlocksMap[c.String()] = &clientBlock{
				lastUpdate:       time.Now(),
				originalRequests: originalRequests,
				requestSlices:    map[uint32][]byte{},
				nextOffset:       0,
				addr:             c,
				host:             nil,
				conn:             nil,
			}
			go processClientRequest(c.String())
		}
		cblockmu.Unlock()
	}
}
