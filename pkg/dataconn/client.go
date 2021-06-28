package dataconn

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/sirupsen/logrus"

	journal "github.com/longhorn/sparse-tools/stats"
)

var (
	//ErrRWTimeout r/w operation timeout
	ErrRWTimeout = errors.New("r/w timeout")
)

const (
	IoTimeout        = 8 * time.Second
	IoTickerInterval = 1 * time.Second
)

//Client replica client
type Client struct {
	end       chan struct{}
	requests  chan *Message
	send      chan *Message
	responses chan *Message
	seq       uint32
	messages  map[uint32]*Message
	wire      *Wire
	peerAddr  string
}

//NewClient replica client
func NewClient(conn net.Conn) *Client {
	c := &Client{
		wire:      NewWire(conn),
		peerAddr:  conn.RemoteAddr().String(),
		end:       make(chan struct{}, 1024),
		requests:  make(chan *Message, 1024),
		send:      make(chan *Message, 1024),
		responses: make(chan *Message, 1024),
		messages:  map[uint32]*Message{},
	}
	go c.loop()
	go c.write()
	go c.read()
	return c
}

//TargetID operation target ID
func (c *Client) TargetID() string {
	return c.peerAddr
}

//WriteAt replica client
func (c *Client) WriteAt(buf []byte, offset int64) (int, error) {
	return c.operation(TypeWrite, buf, offset)
}

//SetError replica client transport error
func (c *Client) SetError(err error) {
	c.responses <- &Message{
		transportErr: err,
	}
}

//ReadAt replica client
func (c *Client) ReadAt(buf []byte, offset int64) (int, error) {
	return c.operation(TypeRead, buf, offset)
}

//Ping replica client
func (c *Client) Ping() error {
	_, err := c.operation(TypePing, nil, 0)
	return err
}

func (c *Client) operation(op uint32, buf []byte, offset int64) (int, error) {
	msg := Message{
		Complete: make(chan struct{}, 1),
		Type:     op,
		Offset:   offset,
		Size:     uint32(len(buf)),
		Data:     nil,
	}

	if op == TypeWrite {
		msg.Data = buf
	}

	c.requests <- &msg

	<-msg.Complete
	// Only copy the message if a read is requested
	if op == TypeRead && (msg.Type == TypeResponse || msg.Type == TypeEOF) {
		copy(buf, msg.Data)
	}
	if msg.Type == TypeError {
		return 0, errors.New(string(msg.Data))
	}
	if msg.Type == TypeEOF {
		return int(msg.Size), io.EOF
	}
	return int(msg.Size), nil
}

//Close replica client
func (c *Client) Close() {
	c.wire.Close()
	c.end <- struct{}{}
}

func (c *Client) loop() {
	defer close(c.send)
	ticker := time.NewTicker(IoTickerInterval)
	defer ticker.Stop()

	var clientError error
	ioInflight := 0
	ioTicksForTimeout := int(IoTimeout / IoTickerInterval)
	ioTicks := 0

	// handleClientError cleans up all in flight messages
	// also stores the error so that future requests/responses get errored immediately.
	handleClientError := func(err error) {
		clientError = err
		for _, msg := range c.messages {
			c.replyError(msg, err)
		}

		ioInflight = 0
		ioTicks = 0
	}

	for {
		select {
		case <-c.end:
			return
		case <-ticker.C:

			if ioInflight > 0 {
				ioTicks++

				if ioTicks <= ioTicksForTimeout {
					continue
				}

				logrus.Errorf("R/W Timeout. No response received in %v", IoTimeout)
				handleClientError(ErrRWTimeout)
				journal.PrintLimited(1000)
			}

		case req := <-c.requests:
			if clientError != nil {
				c.replyError(req, clientError)
				continue
			}

			if req.Type == TypeRead || req.Type == TypeWrite {
				if ioInflight == 0 {
					ioTicks = 0
				}
				ioInflight++
			}

			c.handleRequest(req)
		case resp := <-c.responses:
			if resp.transportErr != nil {
				handleClientError(resp.transportErr)
				continue
			}

			req, pending := c.messages[resp.Seq]
			if !pending {
				logrus.Warnf("received response message id %v seq %v type %v for non pending request", resp.ID, resp.Seq, resp.Type)
				continue
			}

			if req.Type == TypeRead || req.Type == TypeWrite {
				ioInflight--
				ioTicks = 0
			}

			if clientError != nil {
				c.replyError(req, clientError)
				continue
			}

			c.handleResponse(resp)
		}
	}
}

func (c *Client) nextSeq() uint32 {
	c.seq++
	return c.seq
}

func (c *Client) replyError(req *Message, err error) {
	journal.RemovePendingOp(req.ID, false)
	delete(c.messages, req.Seq)
	req.Type = TypeError
	req.Data = []byte(err.Error())
	req.Complete <- struct{}{}
}

func (c *Client) handleRequest(req *Message) {
	switch req.Type {
	case TypeRead:
		req.ID = journal.InsertPendingOp(time.Now(), c.TargetID(), journal.OpRead, int(req.Size))
	case TypeWrite:
		req.ID = journal.InsertPendingOp(time.Now(), c.TargetID(), journal.OpWrite, int(req.Size))
	case TypePing:
		req.ID = journal.InsertPendingOp(time.Now(), c.TargetID(), journal.OpPing, 0)
	}

	req.MagicVersion = MagicVersion
	req.Seq = c.nextSeq()
	c.messages[req.Seq] = req
	c.send <- req
}

func (c *Client) handleResponse(resp *Message) {
	if req, ok := c.messages[resp.Seq]; ok {
		journal.RemovePendingOp(req.ID, true)
		delete(c.messages, resp.Seq)
		req.Type = resp.Type
		req.Size = resp.Size
		req.Data = resp.Data
		req.Complete <- struct{}{}
	}
}

func (c *Client) write() {
	for msg := range c.send {
		if err := c.wire.Write(msg); err != nil {
			c.responses <- &Message{
				transportErr: err,
			}
		}
	}
}

func (c *Client) read() {
	for {
		msg, err := c.wire.Read()
		if err != nil {
			logrus.Errorf("Error reading from wire: %v", err)
			c.responses <- &Message{
				transportErr: err,
			}
			break
		}
		c.responses <- msg
	}
}
