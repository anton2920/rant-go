package main

import (
	"runtime"
	"unsafe"
)

type HTTPRequest struct {
	Method  string
	URL     URL
	Version string
}

type HTTPState int

const (
	HTTP_STATE_UNKNOWN HTTPState = iota

	HTTP_STATE_METHOD
	HTTP_STATE_URI
	HTTP_STATE_VERSION
	HTTP_STATE_HEADER

	HTTP_STATE_DONE
)

type HTTPRequestParser struct {
	State   HTTPState
	Request HTTPRequest
}

type HTTPResponse struct {
	/* Buf points to 'ctx.ResponseBuffer.RemainingSlice()'. Used directly for responses with known sizes. */
	Buf []byte
	Pos int

	/* ContentLength points to stack-allocated buffer enough to hold 'Content-Length' header. */
	ContentLength []byte

	/* Body points to stack-allocated 64 KiB buffer. Used only for (*HTTPResponse).WriteResponse() calls. */
	Body []byte

	/* Date points to array with current date in RFC822 format, which updates every second by kevent timer. */
	Date []byte
}

type HTTPContext struct {
	RequestBuffer  CircularBuffer
	ResponseBuffer CircularBuffer

	Parser HTTPRequestParser

	/* Check must be the same as pointer's bit, if context is in use. */
	Check uintptr
}

type HTTPRouter func(w *HTTPResponse, r *HTTPRequest)

const (
	HTTPStatusOK                    = 200
	HTTPStatusBadRequest            = 400
	HTTPStatusNotFound              = 404
	HTTPStatusMethodNotAllowed      = 405
	HTTPStatusRequestTimeout        = 408
	HTTPStatusRequestEntityTooLarge = 413
)

const (
	HTTPResponseBadRequest            = "HTTP/1.1 400 Bad HTTPRequest\r\nContent-Type: text/html\r\nContent-Length: 175\r\nConnection: close\r\n\r\n<!DOCTYPE html><head><title>400 Bad HTTPRequest</title></head><body><h1>400 Bad HTTPRequest</h1><p>Your browser sent a request that this server could not understand.</p></body></html>"
	HTTPResponseNotFound              = "HTTP/1.1 404 Not Found\r\nContent-Type: text/html\r\nContent-Length: 152\r\nConnection: close\r\n\r\n<!DOCTYPE html><head><title>404 Not Found</title></head><body><h1>404 Not Found</h1><p>The requested URL was not found on this server.</p></body></html>"
	HTTPResponseMethodNotAllowed      = "HTTP/1.1 405 Method Not Allowed\r\nContent-Type: text/html\r\nContent-Length: ...\r\nConnection: close\r\n\r\n"
	HTTPResponseRequestTimeout        = "HTTP/1.1 408 HTTPRequest Timeout\r\nContent-Type: text/html\r\nContent-Length: ...\r\nConnection: close\r\n\r\n"
	HTTPResponseRequestEntityTooLarge = "HTTP/1.1 413 HTTPRequest Entity Too Large\r\nContent-Type: text/html\r\nConent-Length: ...\r\nConnection: close\r\n\r\n"
)

func (w *HTTPResponse) AppendContentLength(contentLength int) {
	var buf [10]byte
	n := SlicePutPositiveInt(unsafe.Slice(&buf[0], len(buf)), contentLength)
	w.Pos += copy(w.Buf[w.Pos:], "\r\nContent-Length: ")
	w.Pos += copy(w.Buf[w.Pos:], unsafe.Slice(&buf[0], n))
}

func (w *HTTPResponse) Start(code int, contentType string) {
	switch code {
	case HTTPStatusOK:
		const statusLine = "HTTP/1.1 200 OK\r\nHost: rant\r\nDate: "
		w.Pos += copy(w.Buf[w.Pos:], statusLine)
		w.Pos += copy(w.Buf[w.Pos:], w.Date)
		w.Pos += copy(w.Buf[w.Pos:], "\r\nContent-Type: ")
		w.Pos += copy(w.Buf[w.Pos:], contentType)
	default:
		println(code)
		panic("unknown status code; for errors use (*HTTPResponse).WriteBuiltinResponse()")
	}
}

func (w *HTTPResponse) StartWithSize(code int, contentType string, contentLength int) {
	w.Start(code, contentType)
	w.AppendContentLength(contentLength)
	w.Pos += copy(w.Buf[w.Pos:], "\r\n\r\n")
}

func (w *HTTPResponse) Finish() {
	w.AppendContentLength(len(w.Body))
	w.Pos += copy(w.Buf[w.Pos:], "\r\n\r\n")
	w.Pos += copy(w.Buf[w.Pos:], w.Body)
}

func (w *HTTPResponse) WriteComplete(code int, contentType string, body []byte) {
	w.StartWithSize(code, contentType, len(body))
	w.Pos += copy(w.Buf[w.Pos:], body)
}

func (w *HTTPResponse) WritePart(buf []byte) {
	w.Pos += copy(w.Buf[w.Pos:], buf)
}

func (w *HTTPResponse) WriteUnfinished(buf []byte) {
	w.Body = append(w.Body, buf...)
}

func (w *HTTPResponse) WriteBuiltinError(code int) {
	switch code {
	case HTTPStatusBadRequest:
		w.Pos += copy(w.Buf[w.Pos:], unsafe.Slice(unsafe.StringData(HTTPResponseBadRequest), len(HTTPResponseBadRequest)))
	case HTTPStatusNotFound:
		w.Pos += copy(w.Buf[w.Pos:], unsafe.Slice(unsafe.StringData(HTTPResponseNotFound), len(HTTPResponseNotFound)))
	}
}

/* NOTE(anton2920): Noescape hides a pointer from escape analysis. Noescape is the identity function but escape analysis doesn't think the output depends on the input. Noescape is inlined and currently compiles down to zero instructions. */
//go:nosplit
func Noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func NewHTTPContext() unsafe.Pointer {
	var err error

	c := new(HTTPContext)
	c.Parser.State = HTTP_STATE_METHOD

	if c.RequestBuffer, err = NewCircularBuffer(PageSize); err != nil {
		println("ERROR: failed to create request buffer:", err.Error())
		return nil
	}
	if c.ResponseBuffer, err = NewCircularBuffer(2 * HugePageSize); err != nil {
		println("ERROR: failed to create response buffer:", err.Error())
		return nil
	}

	return unsafe.Pointer(c)
}

func GetContextAndCheck(ptr unsafe.Pointer) (*HTTPContext, uintptr) {
	uptr := uintptr(ptr)

	check := uptr & 0x1
	ctx := (*HTTPContext)(unsafe.Pointer(uptr - check))

	return ctx, check
}

func HTTPHandleRequests(wBuf *CircularBuffer, rBuf *CircularBuffer, rp *HTTPRequestParser, date []byte, router HTTPRouter) {
	var w HTTPResponse
	w.Body = make([]byte, 64*1024)

	r := &rp.Request

	for {
		for rp.State != HTTP_STATE_DONE {
			switch rp.State {
			default:
				println(rp.State)
				panic("unknown HTTP parser state")
			case HTTP_STATE_UNKNOWN:
				unconsumed := rBuf.UnconsumedString()
				if len(unconsumed) < 2 {
					return
				}
				if unconsumed[:2] == "\r\n" {
					rBuf.Consume(len("\r\n"))
					rp.State = HTTP_STATE_DONE
				} else {
					rp.State = HTTP_STATE_HEADER
				}

			case HTTP_STATE_METHOD:
				unconsumed := rBuf.UnconsumedString()
				if len(unconsumed) < 3 {
					return
				}
				switch unconsumed[:3] {
				case "GET":
					r.Method = "GET"
				default:
					copy(wBuf.RemainingSlice(), unsafe.Slice(unsafe.StringData(HTTPResponseMethodNotAllowed), len(HTTPResponseMethodNotAllowed)))
					rBuf.Reset()
					wBuf.Produce(len(HTTPResponseMethodNotAllowed))
					return
				}
				rBuf.Consume(len(r.Method) + 1)
				rp.State = HTTP_STATE_URI
			case HTTP_STATE_URI:
				unconsumed := rBuf.UnconsumedString()
				lineEnd := FindChar(unconsumed, '\r')
				if lineEnd == -1 {
					return
				}

				uriEnd := FindChar(unconsumed[:lineEnd], ' ')
				if uriEnd == -1 {
					copy(wBuf.RemainingSlice(), unsafe.Slice(unsafe.StringData(HTTPResponseBadRequest), len(HTTPResponseBadRequest)))
					rBuf.Reset()
					wBuf.Produce(len(HTTPResponseBadRequest))
					return
				}

				queryStart := FindChar(unconsumed[:lineEnd], '?')
				if queryStart != -1 {
					r.URL.Path = unconsumed[:queryStart]
					r.URL.Query = unconsumed[queryStart+1 : uriEnd]
				} else {
					r.URL.Path = unconsumed[:uriEnd]
					r.URL.Query = ""
				}

				const httpVersionPrefix = "HTTP/"
				httpVersion := unconsumed[uriEnd+1 : lineEnd]
				if httpVersion[:len(httpVersionPrefix)] != httpVersionPrefix {
					copy(wBuf.RemainingSlice(), unsafe.Slice(unsafe.StringData(HTTPResponseBadRequest), len(HTTPResponseBadRequest)))
					rBuf.Reset()
					wBuf.Produce(len(HTTPResponseBadRequest))
					return
				}
				r.Version = httpVersion[len(httpVersionPrefix):]
				rBuf.Consume(len(r.URL.Path) + len(r.URL.Query) + 1 + len(httpVersionPrefix) + len(r.Version) + len("\r\n"))
				rp.State = HTTP_STATE_UNKNOWN
			case HTTP_STATE_HEADER:
				unconsumed := rBuf.UnconsumedString()
				lineEnd := FindChar(unconsumed, '\r')
				if lineEnd == -1 {
					return
				}
				header := unconsumed[:lineEnd]
				rBuf.Consume(len(header) + len("\r\n"))
				rp.State = HTTP_STATE_UNKNOWN
			}
		}

		w.Buf = wBuf.RemainingSlice()
		w.Body = w.Body[:0]
		w.Date = date
		router((*HTTPResponse)(Noescape(unsafe.Pointer(&w))), r)
		wBuf.Produce(w.Pos)
		w.Pos = 0

		// println("Executed:", r.Method, r.URL.Path, r.URL.Query, string(w.Buf[:13]), w.Pos)

		rp.State = HTTP_STATE_METHOD
	}
}

func HTTPWorker(l int32, router HTTPRouter) {
	var pinner runtime.Pinner
	var events [256]Kevent_t
	var tp Timespec
	var date []byte

	runtime.LockOSThread()

	kq, err := Kqueue()
	if err != nil {
		FatalError("Failed to open kernel queue: ", err)
	}
	chlist := [...]Kevent_t{
		{Ident: uintptr(l), Filter: EVFILT_READ, Flags: EV_ADD | EV_CLEAR},
		{Ident: 1, Filter: EVFILT_TIMER, Flags: EV_ADD, Fflags: NOTE_SECONDS, Data: 1},
	}
	if _, err := Kevent(kq, unsafe.Slice(&chlist[0], len(chlist)), nil, nil); err != nil {
		FatalError("Failed to add event for listener socket: ", err)
	}

	if err := ClockGettime(CLOCK_REALTIME, &tp); err != nil {
		FatalError("Failed to get current walltime: ", err)
	}
	tp.Nsec = 0 /* NOTE(anton2920): we don't care about nanoseconds. */
	date = make([]byte, 31)

	ctxPool := NewPool(NewHTTPContext)

	for {
		nevents, err := Kevent(kq, nil, unsafe.Slice(&events[0], len(events)), nil)
		if err != nil {
			code := err.(E).Code
			if code == EINTR {
				continue
			}
			println("ERROR: failed to get requested kernel events: ", code)
		}
		for i := 0; i < nevents; i++ {
			e := events[i]
			c := int32(e.Ident)

			// println("EVENT", e.Ident, e.Filter, e.Fflags&0xF, e.Data)

			switch c {
			case l:
				c, err := Accept(l, nil, nil)
				if err != nil {
					println("ERROR: failed to accept new connection:", err.Error())
					continue
				}

				ctx := (*HTTPContext)(ctxPool.Get())
				if ctx == nil {
					Fatal("Failed to acquire new HTTP context")
				}
				pinner.Pin(ctx)

				udata := unsafe.Pointer(uintptr(unsafe.Pointer(ctx)) | ctx.Check)
				events := [...]Kevent_t{
					{Ident: uintptr(c), Filter: EVFILT_READ, Flags: EV_ADD | EV_CLEAR, Udata: udata},
					{Ident: uintptr(c), Filter: EVFILT_WRITE, Flags: EV_ADD | EV_CLEAR, Udata: udata},
				}
				if _, err := Kevent(kq, unsafe.Slice(&events[0], len(events)), nil, nil); err != nil {
					println("ERROR: failed to add new events to kqueue:", err.Error())
					ctxPool.Put(unsafe.Pointer(ctx))
				}
				continue
			case 1:
				tp.Sec += int64(e.Data)
				SlicePutTmRFC822(date, TimeToTm(int(tp.Sec)))
				continue
			}

			ctx, check := GetContextAndCheck(e.Udata)
			if check != ctx.Check {
				continue
			}

			switch e.Filter {
			case EVFILT_READ:
				if (e.Flags & EV_EOF) != 0 {
					goto closeConnection
				}

				rBuf := &ctx.RequestBuffer
				wBuf := &ctx.ResponseBuffer
				parser := &ctx.Parser

				if rBuf.RemainingSpace() == 0 {
					Shutdown(c, SHUT_RD)
					Write(c, unsafe.Slice(unsafe.StringData(HTTPResponseRequestEntityTooLarge), len(HTTPResponseRequestEntityTooLarge)))
					goto closeConnection
				}

				n, err := Read(c, rBuf.RemainingSlice())
				if err != nil {
					println("ERROR: failed to read data from socket:", err.Error())
					goto closeConnection
				}
				rBuf.Produce(int(n))

				HTTPHandleRequests(wBuf, rBuf, parser, date, router)
				n, err = Write(c, wBuf.UnconsumedSlice())
				if err != nil {
					println("ERROR: failed to write data to socket:", err.Error())
					goto closeConnection
				}
				wBuf.Consume(int(n))
			case EVFILT_WRITE:
				if (e.Flags & EV_EOF) != 0 {
					goto closeConnection
				}

				wBuf := &ctx.ResponseBuffer
				if wBuf.UnconsumedLen() > 0 {
					n, err := Write(c, wBuf.UnconsumedSlice())
					if err != nil {
						println("ERROR: failed to write data to socket:", err.Error())
						goto closeConnection
					}
					wBuf.Consume(int(n))
				}
			}
			continue

		closeConnection:
			ctx.Check = 1 - ctx.Check
			ctx.RequestBuffer.Reset()
			ctxPool.Put(unsafe.Pointer(ctx))
			Shutdown(c, SHUT_WR)
			Close(c)
			continue
		}
	}
}

func ListenAndServe(port uint16, router HTTPRouter) error {
	l, err := Socket(PF_INET, SOCK_STREAM, 0)
	if err != nil {
		return err
	}

	var enable int32 = 1
	if err = Setsockopt(l, SOL_SOCKET, SO_REUSEPORT, unsafe.Pointer(&enable), uint32(unsafe.Sizeof(enable))); err != nil {
		return err
	}

	addr := SockAddrIn{Family: AF_INET, Addr: INADDR_ANY, Port: SwapBytesInWord(port)}
	if err := Bind(l, &addr, uint32(unsafe.Sizeof(addr))); err != nil {
		return err
	}

	const backlog = 128
	if err := Listen(l, backlog); err != nil {
		return err
	}

	// nworkers := runtime.GOMAXPROCS(0) / 2
	const nworkers = 1
	for i := 0; i < nworkers-1; i++ {
		go HTTPWorker(l, router)
	}
	HTTPWorker(l, router)

	return nil
}
