package serframe

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/knieriem/modbus"
)

type Stream struct {
	buf         []byte
	echo        []byte
	req         chan []byte
	done        chan readResult
	errC        chan error
	eof         bool
	Forward     io.Writer
	MsgComplete func([]byte) bool
	CheckBytes  func(bnew []byte, msg []byte) bool
}

type ReadFunc func() ([]byte, error)

func NewStream(rf ReadFunc, exitC chan<- error) *Stream {
	s := new(Stream)
	s.buf = make([]byte, 0, 256)
	s.req = make(chan []byte)
	s.done = make(chan readResult)
	s.errC = make(chan error)
	go s.handle(rf, exitC)
	return s
}

func (s *Stream) Start() (err error) {
	if s.eof {
		err = io.EOF
		return
	}
	select {
	case err = <-s.errC:
		s.eof = true
	default:
		s.buf = s.buf[:0]
		s.req <- s.buf
	}
	return
}

func (s *Stream) Cancel() {
	s.req <- nil
}

func (s *Stream) Read(ctx context.Context, tMax, interframeTimeoutMax time.Duration) (buf []byte, err error) {
	if s.eof {
		err = io.EOF
		return
	}
	bufok := false
	if s.CheckBytes == nil {
		bufok = true
	}
	nto := 0
	interframeTimeout := 1750 * time.Microsecond
	ntoMax := int((interframeTimeoutMax + interframeTimeout - 1) / interframeTimeout)
	timeout := time.NewTimer(tMax)
	nSkip := 0
readLoop:
	for {
		select {
		case r := <-s.done:
			nb := len(s.buf)
			s.buf = r.data
			if s.CheckBytes != nil && s.echo == nil {
				bufok = s.CheckBytes(s.buf[nb:], s.buf[nSkip:])
			}
			if !timeout.Stop() {
				<-timeout.C
			}
			if r.err != nil {
				err = r.err
				close(s.req)
				s.eof = true
				return
			}
		reeval:
			if s.echo != nil {
				nEcho := len(s.echo)
				if len(s.buf) >= nEcho {
					tail := s.buf[nEcho:]
					if s.CheckBytes != nil && len(tail) != 0 {
						bufok = s.CheckBytes(tail, s.buf[nSkip:])
					}
					if !bytes.Equal(s.buf[:nEcho], s.echo) {
						err = modbus.ErrEchoMismatch
						break readLoop
					}
					nSkip = nEcho
					s.echo = nil
					if len(tail) != 0 {
						goto reeval
					}
				}
				timeout.Reset(tMax)
				break
			} else if s.MsgComplete != nil {
				if s.MsgComplete(s.buf[nSkip:]) {
					break readLoop
				}
			}
			if interframeTimeoutMax == 0 {
				break readLoop
			}
			nto = 0
			timeout.Reset(interframeTimeout)

		case <-timeout.C:
			if s.echo != nil {
				if len(s.buf[nSkip:]) != 0 {
					err = modbus.ErrInvalidEchoLen
				} else {
					err = modbus.ErrTimeout
				}
			} else if len(s.buf[nSkip:]) != 0 && !bufok && nto < ntoMax {
				nto++
				timeout.Reset(interframeTimeout)
				continue
			}
			break readLoop
		case <-ctx.Done():
			err = ctx.Err()
			break readLoop
		}
	}
	s.req <- nil

	buf = s.buf[nSkip:]
	if err == nil && len(buf) == 0 {
		err = modbus.ErrTimeout
	}
	return
}

type readResult struct {
	data []byte
	err  error
}

func (s *Stream) handle(read ReadFunc, exitC chan<- error) {
	var termErr error
	var dest []byte
	var errC chan<- error

	data := make(chan readResult)
	go func() {
		var err error
		for {
			buf, err1 := read()
			data <- readResult{buf, err1}
			<-data
			if err1 != nil {
				err = err1
				break
			}
		}
		close(data)
		exitC <- err
	}()

loop:
	for {
		select {
		case errC <- termErr:
			close(errC)
			break loop
		case dest = <-s.req:
		case r, dataOk := <-data:
			if dest != nil {
				if r.err == nil {
					dest = append(dest, r.data...)
				}
			} else if s.Forward != nil {
				s.Forward.Write(r.data)
			}
			if dataOk {
				data <- readResult{}
			} else {
				data = nil
			}
			if dest != nil {
				select {
				case s.done <- readResult{dest, r.err}:
					if r.err != nil {
						break loop
					}
				case b := <-s.req:
					if s.Forward != nil {
						s.Forward.Write(dest)
					}
					dest = b
				}
			}
			if r.err != nil && errC == nil {
				termErr = r.err
				errC = s.errC
			}
		}
	}
	close(s.done)
}
