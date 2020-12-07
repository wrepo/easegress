package context

import (
	stdcontext "context"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/tracing"
	"github.com/megaease/easegateway/pkg/util/httpheader"
	"github.com/megaease/easegateway/pkg/util/httpstat"
	"github.com/megaease/easegateway/pkg/util/stringtool"
	"github.com/megaease/easegateway/pkg/util/texttemplate"
	"github.com/megaease/easegateway/pkg/util/timetool"
	"github.com/opentracing/opentracing-go"
)

type (
	// HTTPContext is all context of an HTTP processing.
	// It is not goroutine-safe, callers must use Lock/Unlock
	// to protect it by themselves.
	HTTPContext interface {
		Lock()
		Unlock()

		Span() tracing.Span

		Request() HTTPRequest
		Response() HTTPReponse

		stdcontext.Context
		Cancel(err error)
		Cancelled() bool
		ClientDisconnected() bool

		Duration() time.Duration // For log, sample, etc.
		OnFinish(func())         // For setting final client statistics, etc.
		AddTag(tag string)       // For debug, log, etc.

		StatMetric() *httpstat.Metric
		Log() string

		Finish()

		Template() texttemplate.TemplateEngine
		SaveReqToTemplate(pluginName string) error
		SaveRspToTemplate(pluginName string) error
	}

	// HTTPRequest is all operations for HTTP request.
	HTTPRequest interface {
		RealIP() string

		Method() string
		SetMethod(method string)

		// URL
		Scheme() string
		Host() string
		Path() string
		SetPath(path string)
		EscapedPath() string
		Query() string
		SetQuery(query string)
		Fragment() string

		Proto() string

		Header() *httpheader.HTTPHeader
		Cookie(name string) (*http.Cookie, error)
		Cookies() []*http.Cookie
		AddCookie(cookie *http.Cookie)

		Body() io.Reader
		SetBody(io.Reader)

		Std() *http.Request

		Size() uint64 // bytes
	}

	// HTTPReponse is all operations for HTTP response.
	HTTPReponse interface {
		StatusCode() int // Default is 200
		SetStatusCode(code int)

		Header() *httpheader.HTTPHeader
		SetCookie(cookie *http.Cookie)

		SetBody(body io.Reader)
		Body() io.Reader
		OnFlushBody(func(body []byte, complete bool) (newBody []byte))

		Std() http.ResponseWriter

		Size() uint64 // bytes
	}

	// FinishFunc is the type of function to be called back
	// when HTTPContext is finishing.
	FinishFunc = func()

	httpContext struct {
		mutex sync.Mutex

		startTime   *time.Time
		endTime     *time.Time
		finishFuncs []FinishFunc
		tags        []string

		r *httpRequest
		w *httpResponse

		ht             *HTTPTemplate
		tracer         opentracing.Tracer
		span           tracing.Span
		originalReqCtx stdcontext.Context
		stdctx         stdcontext.Context
		cancelFunc     stdcontext.CancelFunc
		err            error
	}
)

// New creates an HTTPContext.
// NOTE: We can't use sync.Pool to recycle context.
// Reference: https://github.com/gin-gonic/gin/issues/1731
func New(stdw http.ResponseWriter, stdr *http.Request,
	tracer *tracing.Tracing, spanName string) HTTPContext {
	originalReqCtx := stdr.Context()
	stdctx, cancelFunc := stdcontext.WithCancel(originalReqCtx)
	stdr = stdr.WithContext(stdctx)

	startTime := time.Now()
	return &httpContext{
		startTime:      &startTime,
		tracer:         tracer,
		span:           tracing.NewSpan(tracer, spanName),
		originalReqCtx: originalReqCtx,
		stdctx:         stdctx,
		cancelFunc:     cancelFunc,
		r:              newHTTPRequest(stdr),
		w:              newHTTPResponse(stdw, stdr),
		ht:             newHTTPTemplate(),
	}
}

func (ctx *httpContext) Lock() {
	ctx.mutex.Lock()
}

func (ctx *httpContext) Unlock() {
	ctx.mutex.Unlock()
}

func (ctx *httpContext) Span() tracing.Span {
	return ctx.span
}

func (ctx *httpContext) AddTag(tag string) {
	ctx.tags = append(ctx.tags, tag)
}

func (ctx *httpContext) Request() HTTPRequest {
	return ctx.r
}

func (ctx *httpContext) Response() HTTPReponse {
	return ctx.w
}

func (ctx *httpContext) Deadline() (time.Time, bool) {
	return ctx.stdctx.Deadline()
}

func (ctx *httpContext) Done() <-chan struct{} {
	return ctx.stdctx.Done()
}

func (ctx *httpContext) Err() error {
	if ctx.err != nil {
		return ctx.err
	}

	return ctx.stdctx.Err()
}

func (ctx *httpContext) Value(key interface{}) interface{} {
	return ctx.stdctx.Value(key)
}

func (ctx *httpContext) Cancel(err error) {
	if !ctx.Cancelled() {
		ctx.AddTag(stringtool.Cat("cancelErr: ", err.Error()))
		ctx.err = err
		ctx.cancelFunc()
	}
}

func (ctx *httpContext) OnFinish(fn FinishFunc) {
	ctx.finishFuncs = append(ctx.finishFuncs, fn)
}

func (ctx *httpContext) Cancelled() bool {
	return ctx.err != nil || ctx.stdctx.Err() != nil
}

func (ctx *httpContext) Duration() time.Duration {
	if ctx.endTime != nil {
		return ctx.endTime.Sub(*ctx.startTime)
	}

	return time.Now().Sub(*ctx.startTime)
}

func (ctx *httpContext) ClientDisconnected() bool {
	return ctx.originalReqCtx.Err() != nil
}

func (ctx *httpContext) Finish() {
	if ctx.ClientDisconnected() {
		ctx.AddTag(fmt.Sprintf("client closed connection: change code %d to 499",
			ctx.w.StatusCode()))
		ctx.w.SetStatusCode(499 /* consistent with nginx */)
	}

	ctx.r.finish()
	ctx.w.finish()

	endTime := time.Now()
	ctx.endTime = &endTime

	for _, fn := range ctx.finishFuncs {
		func() {
			defer func() {
				if err := recover(); err != nil {
					logger.Errorf("failed to handle finish actions for %s: %v, stack trace: \n%s\n",
						ctx.Request().Path(), err, debug.Stack())
				}
			}()

			fn()
		}()
	}

	logger.HTTPAccess(ctx.Log())
}

func (ctx *httpContext) StatMetric() *httpstat.Metric {
	return &httpstat.Metric{
		StatusCode: ctx.Response().StatusCode(),
		Duration:   ctx.Duration(),
		ReqSize:    ctx.Request().Size(),
		RespSize:   ctx.Response().Size(),
	}
}

func (ctx *httpContext) Log() string {
	stdr := ctx.r.std

	// log format:
	// [startTime]
	// [requestInfo]
	// [contextStatistics]
	// [tags]
	//
	// [$startTime]
	// [$remoteAddr $realIP $method $requestURL $proto $statusCode]
	// [$contextDuration $readBytes $writeBytes]
	// [$tags]
	return fmt.Sprintf("[%s] "+
		"[%s %s %s %s %s %d] "+
		"[%v rx:%dB tx:%dB] "+
		"[%s]",
		ctx.startTime.Format(timetool.RFC3339Milli),
		stdr.RemoteAddr, ctx.r.RealIP(), stdr.Method, stdr.RequestURI, stdr.Proto, ctx.w.code,
		ctx.Duration(), ctx.r.Size(), ctx.w.Size(),
		strings.Join(ctx.tags, " | "))
}

// Template returns HTTPTemplate rely interface
func (ctx *httpContext) Template() texttemplate.TemplateEngine {
	return ctx.ht.Engine
}

// SaveHTTPReqToTemplate stores http request related info into HTTP template engine
func (ctx *httpContext) SaveReqToTemplate(pluginName string) error {
	return ctx.ht.SaveRequest(pluginName, ctx)
}

// SaveHTTPRspToTemplate stores http response related info into HTTP template engine
func (ctx *httpContext) SaveRspToTemplate(pluginName string) error {
	return ctx.ht.SaveResponse(pluginName, ctx)
}
