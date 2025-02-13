package serframe

import (
	"bytes"
	"context"
	"io"
	"time"
)

// Stream is an object byte frames can be read from.
type Stream struct {
	buf          []byte
	eof          bool
	globalParams receptionParams
	curParams    receptionParams

	// internal read handler
	req   chan []byte
	input chan readResult
	eofC  chan struct{}

	readBytesInternal func() ([]byte, error)
	internalBufSize   int
	forward           io.Writer

	ExitC <-chan error
}

// NewStream creates a new Stream from the given io.Reader.
func NewStream(r io.Reader, opts ...Option) *Stream {
	s := new(Stream)
	s.req = make(chan []byte)
	s.input = make(chan readResult)
	s.eofC = make(chan struct{})

	s.internalBufSize = 64
	for _, o := range opts {
		o(s)
	}
	if s.readBytesInternal == nil {
		buf := make([]byte, s.internalBufSize)
		s.readBytesInternal = func() ([]byte, error) {
			n, err := r.Read(buf)
			if err != nil {
				return nil, err
			}
			return buf[:n], nil
		}
	}

	exitC := make(chan error, 1)
	s.ExitC = exitC

	go s.handle(exitC)
	return s
}

type Option func(*Stream)

// ForwardUnsolicited configures the stream to forward
// bytes received without a prior call to StartReception
// to the given io.Writer.
func ForwardUnsolicited(w io.Writer) Option {
	return func(s *Stream) {
		s.forward = w
	}
}

// WithInternalBufSize sets the size of the internal buffer
// used to read from the underlying io.Reader.
// The default is 64 bytes.
func WithInternalBufSize(n int) Option {
	return func(s *Stream) {
		if n == 0 {
			return
		}
		s.internalBufSize = n
	}
}

// WithInternalReadBytesFunc specifies that bytes should not
// be read from the io.Reader provided as argument to NewStream,
// but rather be read from the given function.
// This is useful when the instance providing bytes does not implement
// an io.Reader interface.
func WithInternalReadBytesFunc(readBytes func() ([]byte, error)) Option {
	return func(s *Stream) {
		s.readBytesInternal = readBytes
	}
}

// WithReceptionOptions allows to specifiy ReceptionOptions
// globally when creating the Stream object. These settings may
// be overridden later.
func WithReceptionOptions(opts ...ReceptionOption) Option {
	return func(s *Stream) {
		s.globalParams.setup(nil, opts...)
	}
}

// StartReception starts the handling of another data frame,
// that will be stored in buf; a byte received after calling
// this function is regarded the first byte of the new frame.
// The actual reception of the frame will be done in the
// following call to ReadFrame.
// Bytes received before StartReception gets called
// are considered unsolicited.
//
// When implementing a request/response scheme,
// e.g. when communicating to a device, StartReception
// should be called just before Writing the request.
// If calling it after the Write it could happen that the call to Write
// has not returned yet, but the first bytes of the device's response
// frame have already been received by the host system;
// subsequently, ReadFrame could miss the first bytes of
// the response frame.
//
// If the underlying io.Reader returned an io.EOF previously,
// StartReception returns io.EOF.
func (s *Stream) StartReception(buf []byte, opts ...ReceptionOption) error {
	if s.eof {
		return io.EOF
	}
	s.curParams.setup(&s.globalParams, opts...)
	s.buf = buf[:0:len(buf)]
	return s.reqRcpt(s.buf)
}

// CancelReception reverts a previous call to StartReception.
// It should be called in case ReadFrame won't be called
// for some reason.
func (s *Stream) CancelReception() {
	s.reqRcpt(nil)
}

func (s *Stream) reqRcpt(buf []byte) error {
	select {
	case <-s.eofC:
		s.eof = true
		return io.EOF
	case s.req <- buf:
	}
	return nil
}

type ReceptionOption func(*receptionParams)

type receptionParams struct {
	tMax                time.Duration
	interByteTimeout    time.Duration
	interByteTimeoutMax time.Duration
	intercept           FrameInterceptor

	expectedEcho             []byte
	echoSkipInitialNullBytes bool
	expectNoReply            bool
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
// -- because of an activated local echo mechanism --,
// before receiving an actual frame.
// ReadFrame will detect, then skip this echo data.
func WithLocalEcho(expectedEcho []byte) ReceptionOption {
	return func(p *receptionParams) {
		p.expectedEcho = expectedEcho
	}
}

// SkipInitialEchoNullBytes skips null bytes received prior
// an echo of sent data. These null bytes may occur when,
// as part of a protocol, a short break condition,
// slightly larger than a zero byte, gets issued on the bus to
// initiate a request. In case this break condition is
// misinterpeted as a zero byte it will show up at the start of
// the received data just before the echoed request.
// If [WithLocalEcho] is used, this option enables detecting
// and removing these zero bytes; protocols not based on
// per-request break conditions won't need it.
func SkipInitialEchoNullBytes() ReceptionOption {
	return func(p *receptionParams) {
		p.echoSkipInitialNullBytes = true
	}
}

// ExpectNoReply configures the reception to expect no reply,
// as it might be the case for broadcast requests.
// ReadFrame will return after the configured initial timeout,
// if no bytes have been received,
// otherwise it will return ErrUnexpectedReply.
func ExpectNoReply() ReceptionOption {
	return func(p *receptionParams) {
		p.expectNoReply = true
	}
}

// ReadFrame reads the next frame from the stream. On success,
// it returns the frame content as a byte slice,
// otherwise an error will be returned.
// The returned slice's underlying buffer is the one passed
// to StartReception.
func (s *Stream) ReadFrame(ctx context.Context, opts ...ReceptionOption) ([]byte, error) {
	var err error
	if s.eof {
		return nil, io.EOF
	}
	par := s.curParams.setup(nil, opts...)
	frameStatus := None
	if par.intercept == nil {
		frameStatus = Complete
	}
	echoSkipInitialNullBytes := false
	if par.echoSkipInitialNullBytes {
		if xe := par.expectedEcho; xe != nil && xe[0] != 0 {
			echoSkipInitialNullBytes = true
		}
	}
	nto := 0
	ntoMax := par.extInterByteTimeoutSteps()
	timeout := newTimer(par.tMax)
	nSkip := 0
readLoop:
	for {
		select {
		case r, ok := <-s.input:
			if !ok {
				s.eof = true
				return nil, io.EOF
			}
			if r.err != nil {
				err = r.err
				break readLoop
			}
			nb := len(s.buf)
			s.buf = r.data
			if par.intercept != nil && par.expectedEcho == nil {
				frameStatus, err = par.intercept(s.buf[nSkip:], s.buf[nb:])
				if err != nil {
					break readLoop
				}
			}
			if !timeout.Stop() {
				<-timeout.C
			}
		reeval:
			if par.expectedEcho != nil {
				echoPrefixLen := 0
				if echoSkipInitialNullBytes && len(s.buf) >= 2 {
					for i, b := range s.buf {
						if b == 0 {
							continue
						}
						if b == par.expectedEcho[0] {
							echoPrefixLen = i
						}
						break
					}
					echoSkipInitialNullBytes = false
				}
				nEcho := echoPrefixLen + len(par.expectedEcho)
				if len(s.buf) >= nEcho {
					tail := s.buf[nEcho:]
					if par.intercept != nil && len(tail) != 0 {
						frameStatus, err = par.intercept(s.buf[nEcho:], tail)
						if err != nil {
							break readLoop
						}
					}
					if !bytes.Equal(s.buf[echoPrefixLen:nEcho], par.expectedEcho) {
						err = &LocalEchoMismatchError{
							Want: par.expectedEcho,
							Got:  s.buf[echoPrefixLen:nEcho],
						}
						break readLoop
					}
					nSkip = nEcho
					par.expectedEcho = nil
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
			if par.expectedEcho != nil {
				if len(s.buf) != 0 {
					if echoSkipInitialNullBytes {
						for i, b := range s.buf {
							if b != 0 {
								err = &LocalEchoMismatchError{
									Want: par.expectedEcho,
									Got:  s.buf[i:],
								}
								break readLoop
							}
						}
						err = ErrTimeout
					}
					err = &LocalEchoMismatchError{
						Want: par.expectedEcho,
						Got:  s.buf,
					}
				} else {
					err = ErrTimeout
				}
			} else if len(s.buf[nSkip:]) != 0 {
				if frameStatus != Complete && nto < ntoMax {
					nto++
					timeout.Reset(par.interByteTimeout)
					continue
				}
				if par.expectNoReply {
					err = ErrUnexpectedReply
				}
			}
			break readLoop
		case <-ctx.Done():
			err = ctx.Err()
			break readLoop
		}
	}
	s.req <- nil

	data := s.buf[nSkip:]
	if err == nil && len(data) == 0 && !par.expectNoReply {
		return nil, ErrTimeout
	}
	return data, err
}

type readResult struct {
	data []byte
	err  error
}

const maxConsecutiveReadErrors = 8

func (s *Stream) handle(exitC chan<- error) {
	var dest []byte

	data := make(chan readResult)
	go func() {
		nErr := 0
		for {
			isEOF := false
			buf, err := s.readBytesInternal()
			if err != nil {
				nErr++
				if err == io.EOF || requiresTermination(err) || nErr > maxConsecutiveReadErrors {
					isEOF = true
					err = nil
				}
			} else {
				nErr = 0
			}
			if len(buf) != 0 {
				data <- readResult{buf, err}
				<-data
			}
			if isEOF {
				close(data)
				exitC <- io.EOF
				return
			}
		}
	}()

loop:
	for {
		select {
		case dest = <-s.req:
		case r, dataOk := <-data:
		again:
			if !dataOk {
				close(s.eofC)
				break loop
			}
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
			data <- readResult{}
			if dest != nil {
				select {
				case r, dataOk = <-data:
					// process newly arrived data before handing data up
					goto again
				case s.input <- readResult{dest, err}:
				case b := <-s.req:
					if s.forward != nil {
						s.forward.Write(dest)
					}
					dest = b
				}
			}
		}
	}
	close(s.input)
}

var ErrTimeout = Error("timeout")
var ErrOverflow = Error("receive buffer overflow")
var ErrUnexpectedReply = Error("unexpected reply")

type Error string

func (e Error) Error() string {
	return "serframe: " + string(e)
}

type LocalEchoMismatchError struct {
	Want, Got []byte
}

func (e *LocalEchoMismatchError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.tooShort() {
		return "invalid local echo length"
	}
	return "local echo mismatch"
}

func (e *LocalEchoMismatchError) tooShort() bool {
	return len(e.Got) < len(e.Want)
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
