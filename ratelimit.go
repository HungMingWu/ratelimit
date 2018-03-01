package ratelimit

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// rl is the global rate limit object.
var rl rateLimit

type (
	// rateLimit declares the global rate limit for read and write operations
	// on a io.ReadWriter. Whenever a caller wants to read or write, they have
	// to wait until readBlock/writeBlock to start the actual read or write
	// operation. Each caller also pushes these timestamps into the future to
	// prevent other callers to read or write prematurely.
	rateLimit struct {
		atomicPacketSize uint64 // the maximum amount of data a caller can read/write at once
		atomicWriteBPS   int64  // the bytes per second that can be written.
		atomicReadBPS    int64  // the bytes per second that can be read.

		wmu        sync.Mutex // locks writeBlock.
		writeBlock time.Time  // timestamp before which no new write can start.

		rmu       sync.Mutex // locks readBlock.
		readBlock time.Time  // timestamp before which no new read can start.
	}
	// rlReadWriter is a simple wrapper for the io.ReadWriter interface.
	rlReadWriter struct {
		io.ReadWriter
		cancel chan struct{}
	}
)

// NewRLReadWriter wraps a io.ReadWriter into a rlReadWriter.
func NewRLReadWriter(rw io.ReadWriter, cancel chan struct{}) io.ReadWriter {
	return &rlReadWriter{
		rw,
		cancel,
	}
}

// NewRLConn wrap a net.Conn into a rlReadWriter.
func NewRLConn(conn net.Conn, cancel chan struct{}) net.Conn {
	return (io.ReadWriter)(&rlReadWriter{
		conn,
		cancel,
	}).(net.Conn)
}

// SetLimits sets new limits for the global rate limiter.
func SetLimits(readBPS, writeBPS int64, packetSize uint64) {
	atomic.StoreInt64(&rl.atomicReadBPS, readBPS)
	atomic.StoreInt64(&rl.atomicWriteBPS, writeBPS)
	atomic.StoreUint64(&rl.atomicPacketSize, packetSize)
}

// Read reads from the underlying readWriter with the maximum possible speed
// allowed by the rateLimit.
func (l *rlReadWriter) Read(b []byte) (n int, err error) {
	packetSize := atomic.LoadUint64(&rl.atomicPacketSize)
	if packetSize == 0 {
		l.readPacket(b)
	}
	for len(b) > 0 {
		var data []byte
		if uint64(len(b)) > packetSize {
			data = b[:packetSize]
			b = b[packetSize:]
		} else {
			data = b
			b = b[:0]
		}
		var read int
		for len(data) > 0 {
			read, err = l.readPacket(data)
			data = data[read:]
			n += read
			if err != nil {
				return
			}
		}
	}
	return
}

// Write writes to the underlying readWriter with the maximum possible speed
// allowed by the rateLimit.
func (l *rlReadWriter) Write(b []byte) (n int, err error) {
	packetSize := atomic.LoadUint64(&rl.atomicPacketSize)
	if packetSize == 0 {
		l.writePacket(b)
	}
	for len(b) > 0 {
		var data []byte
		if uint64(len(b)) > packetSize {
			data = b[:packetSize]
			b = b[packetSize:]
		} else {
			data = b
			b = b[:0]
		}
		var written int
		for len(data) > 0 {
			written, err = l.writePacket(data)
			data = data[written:]
			n += written
			if err != nil {
				return
			}
		}
	}
	return
}

// readPacket is a helper function that reads up to a single packet worth of
// data.
func (l *rlReadWriter) readPacket(b []byte) (n int, err error) {
	// Get the current max bandwidth.
	bps := time.Duration(atomic.LoadInt64(&rl.atomicReadBPS))

	// If bps is 0 there is no limit.
	if bps == 0 {
		return l.ReadWriter.Read(b)
	}

	rl.rmu.Lock()
	// Calculate how long we can take for our read.
	timeForRead := time.Second / bps * time.Duration(len(b))

	// If the readBlock is in the past we reset it to time.Now() +
	// timeForRead. Otherwise we just add to the timestamp.
	wb := rl.readBlock
	if rl.readBlock.After(time.Now()) {
		rl.readBlock = rl.readBlock.Add(timeForRead)
	} else {
		rl.readBlock = time.Now().Add(timeForRead)
	}
	rl.rmu.Unlock()

	// Sleep until it is safe to read.
	select {
	case <-time.After(time.Until(wb)):
	case <-l.cancel:
		return 0, errors.New("read cancelled due to interrupt")
	}
	return l.ReadWriter.Read(b)
}

// writePacket is a helper function that writes up to a single packet worth of
// data.
func (l *rlReadWriter) writePacket(b []byte) (n int, err error) {
	// Get the current max bandwidth.
	bps := time.Duration(atomic.LoadInt64(&rl.atomicWriteBPS))

	// If bps is 0 there is no limit.
	if bps == 0 {
		return l.ReadWriter.Write(b)
	}

	rl.wmu.Lock()
	// Calculate how long we can take for our write.
	timeForWrite := time.Second / bps * time.Duration(len(b))

	// If the writeBlock is in the past we reset it to time.Now() +
	// timeForWrite. Otherwise we just add to the timestamp.
	wb := rl.writeBlock
	if rl.writeBlock.After(time.Now()) {
		rl.writeBlock = rl.writeBlock.Add(timeForWrite)
	} else {
		rl.writeBlock = time.Now().Add(timeForWrite)
	}
	rl.wmu.Unlock()

	// Sleep until it is safe to write.
	select {
	case <-time.After(time.Until(wb)):
	case <-l.cancel:
		return 0, errors.New("write cancelled due to interrupt")
	}
	return l.ReadWriter.Write(b)
}