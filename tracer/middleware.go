package tracer

import (
	"fmt"
	"net/http"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

//TraceMiddleware trace middleware struct
type TraceMiddleware struct {
	Next http.Handler
}

//NewTraceMiddleware create new tracer middleware
func NewTraceMiddleware(next http.Handler) *TraceMiddleware {
	return &TraceMiddleware{
		Next: next,
	}
}

func (mw *TraceMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tr := opentracing.GlobalTracer()
	if tr == nil {
		fmt.Println("No Global tracer defined, skip...")
		mw.Next.ServeHTTP(w, r)
		return
	}
	name := "HTTP " + r.Method + " " + r.URL.Path
	var sp opentracing.Span

	wireContext, err := tr.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	if err != nil {
		sp = tr.StartSpan(name)
	} else {
		sp = tr.StartSpan(name, opentracing.ChildOf(wireContext))
	}
	ext.SpanKind.Set(sp, "handler")
	sp.SetTag("handler.method", r.Method)
	err = sp.Tracer().Inject(sp.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	if err != nil {
		return
	}
	r = r.WithContext(opentracing.ContextWithSpan(r.Context(), sp))
	mw.Next.ServeHTTP(w, r)
	sp.Finish()

}
