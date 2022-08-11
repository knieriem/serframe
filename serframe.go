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
	tMax                time.Duration
	interByteTimeout    time.Duration
	interByteTimeoutMax time.Duration
	intercept           FrameInterceptor
	echo                []byte
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

func (p *receptionParams) extInterByteTimeoutSteps() int {
	ito := p.interByteTimeout
	if ito == 0 {
		return 0
	}
	return int((p.interByteTimeoutMax + ito - 1) / ito)
}

// WithInitialTimeout configures the timeout that is active
// before the first byte of a frame has been received.
// A value of zero deactivates the timeout, which is
// also the default.
func WithInitialTimeout(t time.Duration) ReceptionOption {
	return func(p *receptionParams) {
		p.tMax = t
	}
}

// The InterByteTimeout is the time that is allowed to elapse after
// a byte has been received before a frame is considered complete.
//
// On a pc system there may be the case that this timeout elapsed
// but the goroutine reading bytes from the stream didn't have a
// chance to know about new bytes waiting to be read --
// as a result, frames may appear truncated. To work around this
// problem, while allowing to keep the inter byte timeout short,
// a combination of an extended inter-byte timeout and
// a frame interceptor may be used; see options WithExtInterByteTimeout
// and WithFrameInterceptor.
func WithInterByteTimeout(t time.Duration) ReceptionOption {
	return func(p *receptionParams) {
		p.interByteTimeout = t
	}
}

// WithExtInterByteTimeout extends the duration of the normal inter-byte
// timeout to the specified value, in case a FrameInterceptor has been
// configured. A frame will be considered complete, after both at least the
// normal inter-byte timeout has elapsed, and the interceptor signals Complete.
func WithExtInterByteTimeout(t time.Duration) ReceptionOption {
	return func(p *receptionParams) {
		p.interByteTimeoutMax = t
		if p.interByteTimeout == 0 {
			p.interByteTimeout = t
		}
	}
}

// A FrameInterceptor is called every time new bytes have been received and
// appended to the current frame. The interceptor may infer from the content
// of curFrame, whether it can be considered complete or not,
// and return an corresponding frame status, and, if appropriate, an error.
// NewPart might be used, for example, to calculate a hash in parallel.
type FrameInterceptor func(curFrame, newPart []byte) (FrameStatus, error)

func WithFrameInterceptor(f FrameInterceptor) ReceptionOption {
	return func(p *receptionParams) {
		p.intercept = f
	}
}

type FrameStatus int

const (
	None FrameStatus = iota
	Complete
	CompleteSkipTimeout
)

// WithLocalEcho sets the data that is expected to be received first
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
	frameStatus := None
	if par.intercept == nil {
		frameStatus = Complete
	}
	nto := 0
	ntoMax := par.extInterByteTimeoutSteps()
	timeout := newTimer(par.tMax)
	nSkip := 0
readLoop:
	for {
		select {
		case r := <-s.done:
			nb := len(s.buf)
			s.buf = r.data
			if par.intercept != nil && par.echo == nil {
				frameStatus, err = par.intercept(s.buf[nSkip:], s.buf[nb:])
				if err != nil {
					break readLoop
				}
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
					if par.intercept != nil && len(tail) != 0 {
						frameStatus, err = par.intercept(s.buf[nSkip:], tail)
						if err != nil {
							break readLoop
						}
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
			} else if frameStatus == CompleteSkipTimeout {
				break readLoop
			}
			if par.interByteTimeout == 0 {
				break readLoop
			}
			nto = 0
			timeout.Reset(par.interByteTimeout)

		case <-timeout.C:
			if par.echo != nil {
				if len(s.buf[nSkip:]) != 0 {
					err = ErrInvalidEchoLen
				} else {
					err = ErrTimeout
				}
			} else if len(s.buf[nSkip:]) != 0 && frameStatus != Complete && nto < ntoMax {
				nto++
				timeout.Reset(par.interByteTimeout)
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

// timer wraps a time.Timer, deferring initialization
// in case duration is zero
type timer struct {
	tim *time.Timer
	C   <-chan time.Time
}

func newTimer(d time.Duration) *timer {
	t := new(timer)
	if d != 0 {
		t.tim = time.NewTimer(d)
		t.C = t.tim.C
	}
	return t
}

func (t *timer) Reset(d time.Duration) bool {
	if t.tim == nil {
		if d == 0 {
			return false
		}
		t.tim = time.NewTimer(d)
		t.C = t.tim.C
		return false
	}
	return t.tim.Reset(d)
}

func (t *timer) Stop() bool {
	if t.tim == nil {
		return true
	}
	return t.tim.Stop()
}
