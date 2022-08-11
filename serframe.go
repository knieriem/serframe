package serframe

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/knieriem/modbus"
)

type Stream struct {
	buf          []byte
	eof          bool
	MsgComplete  func([]byte) bool
	globalParams receptionParams
	curParams    receptionParams

	// internal read handler
	req  chan []byte
	done chan readResult
	errC chan error

	r               io.Reader
	internalBuf     []byte
	internalBufSize int
	forward         io.Writer

	ExitC <-chan error
}

func NewStream(r io.Reader, opts ...Option) *Stream {
	s := new(Stream)
	s.req = make(chan []byte)
	s.done = make(chan readResult)
	s.errC = make(chan error)
	s.globalParams.tMax = time.Second
	s.r = r

	s.internalBufSize = 64
	for _, o := range opts {
		o(s)
	}
	s.internalBuf = make([]byte, s.internalBufSize)

	exitC := make(chan error, 1)
	s.ExitC = exitC

	go s.handle(exitC)
	return s
}

type Option func(*Stream)

func ForwardUnsolicited(w io.Writer) Option {
	return func(s *Stream) {
		s.forward = w
	}
}

func WithInternalBufSize(n int) Option {
	return func(s *Stream) {
		if n == 0 {
			return
		}
		s.internalBufSize = n
	}
}

func WithReceptionOptions(opts ...ReceptionOption) Option {
	return func(s *Stream) {
		s.globalParams.setup(nil, opts...)
	}
}

func (s *Stream) StartReception(buf []byte, opts ...ReceptionOption) (err error) {
	if s.eof {
		err = io.EOF
		return
	}
	select {
	case err = <-s.errC:
		s.eof = true
	default:
		s.curParams.setup(&s.globalParams, opts...)
		s.buf = buf[:0:len(buf)]
		s.req <- s.buf
	}
	return
}

func (s *Stream) CancelReception() {
	s.req <- nil
}

type ReceptionOption func(*receptionParams)

type receptionParams struct {
	tMax                 time.Duration
	interframeTimeoutMax time.Duration
	frameValid           FrameValidationFunc
	skipTimeout          bool
	echo                 []byte
}

func (p *receptionParams) setup(dflt *receptionParams, opts ...ReceptionOption) *receptionParams {
	if dflt != nil {
		*p = *dflt
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func WithInitialTimeout(t time.Duration) ReceptionOption {
	return func(p *receptionParams) {
		if t == 0 {
			return
		}
		p.tMax = t
	}
}

func WithInterframeTimeout(t time.Duration) ReceptionOption {
	return func(p *receptionParams) {
		p.interframeTimeoutMax = t
	}
}

type FrameValidationFunc func(bnew []byte, msg []byte) bool

func WithFrameValidation(f FrameValidationFunc) ReceptionOption {
	return func(p *receptionParams) {
		p.frameValid = f
	}
}

func SkipTimeoutIfValid() ReceptionOption {
	return func(p *receptionParams) {
		p.skipTimeout = true
	}
}

func WithLocalEcho(echo []byte) ReceptionOption {
	return func(p *receptionParams) {
		p.echo = echo
	}
}

func (s *Stream) ReadFrame(ctx context.Context, opts ...ReceptionOption) (buf []byte, err error) {
	if s.eof {
		err = io.EOF
		return
	}
	par := s.curParams.setup(&s.globalParams, opts...)
	bufok := false
	if par.frameValid == nil {
		bufok = true
	}
	nto := 0
	interframeTimeout := 1750 * time.Microsecond
	ntoMax := int((par.interframeTimeoutMax + interframeTimeout - 1) / interframeTimeout)
	timeout := time.NewTimer(par.tMax)
	nSkip := 0
readLoop:
	for {
		select {
		case r := <-s.done:
			nb := len(s.buf)
			s.buf = r.data
			if par.frameValid != nil && par.echo == nil {
				bufok = par.frameValid(s.buf[nb:], s.buf[nSkip:])
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
			if par.echo != nil {
				nEcho := len(par.echo)
				if len(s.buf) >= nEcho {
					tail := s.buf[nEcho:]
					if par.frameValid != nil && len(tail) != 0 {
						bufok = par.frameValid(tail, s.buf[nSkip:])
					}
					if !bytes.Equal(s.buf[:nEcho], par.echo) {
						err = modbus.ErrEchoMismatch
						break readLoop
					}
					nSkip = nEcho
					par.echo = nil
					if len(tail) != 0 {
						goto reeval
					}
				}
				timeout.Reset(par.tMax)
				break
			} else if bufok && par.skipTimeout {
				break readLoop
			}
			if par.interframeTimeoutMax == 0 {
				break readLoop
			}
			nto = 0
			timeout.Reset(interframeTimeout)

		case <-timeout.C:
			if par.echo != nil {
				if len(s.buf[nSkip:]) != 0 {
					err = ErrInvalidEchoLen
				} else {
					err = ErrTimeout
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
		err = ErrTimeout
	}
	return
}

type readResult struct {
	data []byte
	err  error
}

func (s *Stream) handle(exitC chan<- error) {
	var termErr error
	var dest []byte
	var errC chan<- error

	data := make(chan readResult)
	go func() {
		var err error
		for {
			buf := s.internalBuf
			n, err1 := s.r.Read(buf)
			if err1 != nil {
				buf = nil
			} else {
				buf = buf[:n]
			}
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
			err := r.err
			if dest != nil {
				if err == nil {
					free := cap(dest) - len(dest)
					if len(r.data) > free {
						r.data = r.data[:free]
						err = ErrOverflow
					}
					dest = append(dest, r.data...)
				}
			} else if s.forward != nil {
				s.forward.Write(r.data)
			}
			if dataOk {
				data <- readResult{}
			} else {
				data = nil
			}
			if dest != nil {
				select {
				case s.done <- readResult{dest, err}:
					if r.err != nil {
						break loop
					}
				case b := <-s.req:
					if s.forward != nil {
						s.forward.Write(dest)
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

var ErrTimeout = Error("timeout")
var ErrEchoMismatch = Error("local echo mismatch")
var ErrInvalidEchoLen = Error("invalid local echo length")
var ErrOverflow = Error("receive buffer overflow")

type Error string

func (e Error) Error() string {
	return "serframe: " + string(e)
}
