package protocol

import (
	"compress/flate"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/calmh/syncthing/buffers"
)

const (
	messageTypeIndex       = 1
	messageTypeRequest     = 2
	messageTypeResponse    = 3
	messageTypePing        = 4
	messageTypePong        = 5
	messageTypeIndexUpdate = 6
)

type FileInfo struct {
	Name     string
	Flags    uint32
	Modified int64
	Blocks   []BlockInfo
}

type BlockInfo struct {
	Length uint32
	Hash   []byte
}

type Model interface {
	// An index was received from the peer node
	Index(nodeID string, files []FileInfo)
	// An index update was received from the peer node
	IndexUpdate(nodeID string, files []FileInfo)
	// A request was made by the peer node
	Request(nodeID, name string, offset uint64, size uint32, hash []byte) ([]byte, error)
	// The peer node closed the connection
	Close(nodeID string)
}

type Connection struct {
	sync.RWMutex

	ID          string
	receiver    Model
	reader      io.Reader
	mreader     *marshalReader
	writer      io.Writer
	mwriter     *marshalWriter
	closed      bool
	awaiting    map[int]chan asyncResult
	nextId      int
	peerLatency time.Duration
	indexSent   map[string]int64

	lastStatistics Statistics
	statisticsLock sync.Mutex

	lastReceive     time.Time
	lastReceiveLock sync.RWMutex
}

var ErrClosed = errors.New("Connection closed")

type asyncResult struct {
	val []byte
	err error
}

const pingTimeout = 30 * time.Second
const pingIdleTime = 5 * time.Minute

func NewConnection(nodeID string, reader io.Reader, writer io.Writer, receiver Model) *Connection {
	flrd := flate.NewReader(reader)
	flwr, err := flate.NewWriter(writer, flate.BestSpeed)
	if err != nil {
		panic(err)
	}

	c := Connection{
		receiver:       receiver,
		reader:         flrd,
		mreader:        &marshalReader{r: flrd},
		writer:         flwr,
		mwriter:        &marshalWriter{w: flwr},
		awaiting:       make(map[int]chan asyncResult),
		lastReceive:    time.Now(),
		ID:             nodeID,
		lastStatistics: Statistics{At: time.Now()},
	}

	go c.readerLoop()
	go c.pingerLoop()

	return &c
}

// Index writes the list of file information to the connected peer node
func (c *Connection) Index(idx []FileInfo) {
	c.Lock()

	var msgType int
	if c.indexSent == nil {
		// This is the first time we send an index.
		msgType = messageTypeIndex

		c.indexSent = make(map[string]int64)
		for _, f := range idx {
			c.indexSent[f.Name] = f.Modified
		}
	} else {
		// We have sent one full index. Only send updates now.
		msgType = messageTypeIndexUpdate
		var diff []FileInfo
		for _, f := range idx {
			if modified, ok := c.indexSent[f.Name]; !ok || f.Modified != modified {
				diff = append(diff, f)
				c.indexSent[f.Name] = f.Modified
			}
		}
		idx = diff
	}

	c.mwriter.writeHeader(header{0, c.nextId, msgType})
	c.mwriter.writeIndex(idx)
	err := c.flush()
	c.nextId = (c.nextId + 1) & 0xfff
	c.Unlock()
	if err != nil || c.mwriter.err != nil {
		c.close()
		return
	}
}

// Request returns the bytes for the specified block after fetching them from the connected peer.
func (c *Connection) Request(name string, offset uint64, size uint32, hash []byte) ([]byte, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	c.Lock()
	rc := make(chan asyncResult)
	c.awaiting[c.nextId] = rc
	c.mwriter.writeHeader(header{0, c.nextId, messageTypeRequest})
	c.mwriter.writeRequest(request{name, offset, size, hash})
	if c.mwriter.err != nil {
		c.Unlock()
		c.close()
		return nil, c.mwriter.err
	}
	err := c.flush()
	if err != nil {
		c.Unlock()
		c.close()
		return nil, err
	}
	c.nextId = (c.nextId + 1) & 0xfff
	c.Unlock()

	res, ok := <-rc
	if !ok {
		return nil, ErrClosed
	}
	return res.val, res.err
}

func (c *Connection) Ping() (time.Duration, bool) {
	if c.isClosed() {
		return 0, false
	}
	c.Lock()
	rc := make(chan asyncResult)
	c.awaiting[c.nextId] = rc
	t0 := time.Now()
	c.mwriter.writeHeader(header{0, c.nextId, messageTypePing})
	err := c.flush()
	if err != nil || c.mwriter.err != nil {
		c.Unlock()
		c.close()
		return 0, false
	}
	c.nextId = (c.nextId + 1) & 0xfff
	c.Unlock()

	_, ok := <-rc
	return time.Since(t0), ok
}

func (c *Connection) Stop() {
}

type flusher interface {
	Flush() error
}

func (c *Connection) flush() error {
	if f, ok := c.writer.(flusher); ok {
		return f.Flush()
	}
	return nil
}

func (c *Connection) close() {
	c.Lock()
	if c.closed {
		c.Unlock()
		return
	}
	c.closed = true
	for _, ch := range c.awaiting {
		close(ch)
	}
	c.awaiting = nil
	c.Unlock()

	c.receiver.Close(c.ID)
}

func (c *Connection) isClosed() bool {
	c.RLock()
	defer c.RUnlock()
	return c.closed
}

func (c *Connection) readerLoop() {
	for !c.isClosed() {
		hdr := c.mreader.readHeader()
		if c.mreader.err != nil {
			c.close()
			break
		}
		if hdr.version != 0 {
			log.Printf("Protocol error: %s: unknown message version %#x", c.ID, hdr.version)
			c.close()
			break
		}

		c.lastReceiveLock.Lock()
		c.lastReceive = time.Now()
		c.lastReceiveLock.Unlock()

		switch hdr.msgType {
		case messageTypeIndex:
			files := c.mreader.readIndex()
			if c.mreader.err != nil {
				c.close()
			} else {
				c.receiver.Index(c.ID, files)
			}

		case messageTypeIndexUpdate:
			files := c.mreader.readIndex()
			if c.mreader.err != nil {
				c.close()
			} else {
				c.receiver.IndexUpdate(c.ID, files)
			}

		case messageTypeRequest:
			c.processRequest(hdr.msgID)
			if c.mreader.err != nil || c.mwriter.err != nil {
				c.close()
			}

		case messageTypeResponse:
			data := c.mreader.readResponse()

			if c.mreader.err != nil {
				c.close()
			} else {
				c.RLock()
				rc, ok := c.awaiting[hdr.msgID]
				c.RUnlock()

				if ok {
					rc <- asyncResult{data, c.mreader.err}
					close(rc)

					c.Lock()
					delete(c.awaiting, hdr.msgID)
					c.Unlock()
				}
			}

		case messageTypePing:
			c.Lock()
			c.mwriter.writeUint32(encodeHeader(header{0, hdr.msgID, messageTypePong}))
			err := c.flush()
			c.Unlock()
			if err != nil || c.mwriter.err != nil {
				c.close()
			}

		case messageTypePong:
			c.RLock()
			rc, ok := c.awaiting[hdr.msgID]
			c.RUnlock()

			if ok {
				rc <- asyncResult{}
				close(rc)

				c.Lock()
				delete(c.awaiting, hdr.msgID)
				c.Unlock()
			}

		default:
			log.Printf("Protocol error: %s: unknown message type %#x", c.ID, hdr.msgType)
			c.close()
		}
	}
}

func (c *Connection) processRequest(msgID int) {
	req := c.mreader.readRequest()
	if c.mreader.err != nil {
		c.close()
	} else {
		go func() {
			data, _ := c.receiver.Request(c.ID, req.name, req.offset, req.size, req.hash)
			c.Lock()
			c.mwriter.writeUint32(encodeHeader(header{0, msgID, messageTypeResponse}))
			c.mwriter.writeResponse(data)
			err := c.flush()
			c.Unlock()
			buffers.Put(data)
			if c.mwriter.err != nil || err != nil {
				c.close()
			}
		}()
	}
}

func (c *Connection) pingerLoop() {
	var rc = make(chan time.Duration, 1)
	for !c.isClosed() {
		c.lastReceiveLock.RLock()
		lr := c.lastReceive
		c.lastReceiveLock.RUnlock()

		if time.Since(lr) > pingIdleTime {
			go func() {
				t, ok := c.Ping()
				if ok {
					rc <- t
				}
			}()
			select {
			case lat := <-rc:
				c.Lock()
				c.peerLatency = (c.peerLatency + lat) / 2
				c.Unlock()
			case <-time.After(pingTimeout):
				c.close()
			}
		}
		time.Sleep(time.Second)
	}
}

type Statistics struct {
	At             time.Time
	InBytesTotal   int
	InBytesPerSec  int
	OutBytesTotal  int
	OutBytesPerSec int
	Latency        time.Duration
}

func (c *Connection) Statistics() Statistics {
	c.statisticsLock.Lock()
	defer c.statisticsLock.Unlock()

	secs := time.Since(c.lastStatistics.At).Seconds()
	rt := int(c.mreader.getTot())
	wt := int(c.mwriter.getTot())
	stats := Statistics{
		At:             time.Now(),
		InBytesTotal:   rt,
		InBytesPerSec:  int(float64(rt-c.lastStatistics.InBytesTotal) / secs),
		OutBytesTotal:  wt,
		OutBytesPerSec: int(float64(wt-c.lastStatistics.OutBytesTotal) / secs),
		Latency:        c.peerLatency,
	}
	c.lastStatistics = stats
	return stats
}